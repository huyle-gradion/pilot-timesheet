package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// Catalog candidates for the classifications judgment (issue #234). The
// deterministic `candidates` step reads the live catalog once per distinct
// topic and writes candidates.json into the day folder, bound to the staged
// ledger digest. The judgment then embeds those closed lists, so the model
// picks from them with no tool call of its own. Each read goes through the
// session's classifications handler, which caches the catalog in state, so
// classify validates the answer against the same catalog_id.
const (
	// maxCandidateQueries bounds the catalog reads per day. It stays below the
	// store's catalog cache size so no prefetched catalog is pruned before
	// classify validates against it.
	maxCandidateQueries = 12
	// maxCandidateOptions bounds the options shown per topic; the total
	// judgment is therefore at most maxCandidateQueries*maxCandidateOptions
	// options however many intervals the day has.
	maxCandidateOptions = 10
	maxQueryRunes       = 80
)

// candidateQuery is the catalog search text for one staged block: the
// recorded description without its inline "PRs:" list, first line only,
// whitespace-collapsed and capped. It is the one place to change the query
// policy.
func candidateQuery(e timesheet.EntryRecord) string {
	q := e.Description
	if i := strings.Index(q, "PRs:"); i >= 0 {
		q = q[:i]
	}
	if i := strings.IndexByte(q, '\n'); i >= 0 {
		q = q[:i]
	}
	q = strings.Join(strings.Fields(q), " ")
	q = strings.TrimRight(q, " .;,-")
	if r := []rune(q); len(r) > maxQueryRunes {
		q = string(r[:maxQueryRunes])
	}
	return q
}

// nextCandidateSet is one catalog read's bounded option list. Complete is
// false when the catalog was filtered or truncated, or when the option list
// was capped here: a partial list cannot prove an option absent.
type nextCandidateSet struct {
	Ref       string                `json:"ref"`
	Query     string                `json:"query"`
	CatalogID string                `json:"catalog_id"`
	Complete  bool                  `json:"complete"`
	Options   []nextCandidateOption `json:"options"`
	Note      string                `json:"note,omitempty"`
}

type nextCandidateOption struct {
	ClassificationID string `json:"classification_id"`
	Label            string `json:"label"`
	Code             string `json:"code,omitempty"`
}

// candidatesFile is candidates.json: the sets plus each interval's set ref.
// An interval missing from Intervals has no candidates (empty description, or
// beyond maxCandidateQueries distinct topics).
type candidatesFile struct {
	LedgerDigest string             `json:"ledger_digest"`
	Sets         []nextCandidateSet `json:"sets"`
	Intervals    map[string]string  `json:"intervals"`
	Note         string             `json:"note,omitempty"`
}

// readCandidatesFile returns the parsed file (nil when absent). An
// unparseable file is present but unbound, so it is simply re-fetched.
func readCandidatesFile(path string) (*candidatesFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f candidatesFile
	if err := json.Unmarshal(data, &f); err != nil {
		return &candidatesFile{}, nil
	}
	return &f, nil
}

// stagedDayBlocks is the day's staged block content and its ledger digest, by
// the same projection the planner's classification judgment binds to.
func stagedDayBlocks(statePath, day string) (map[string]timesheet.EntryRecord, string, error) {
	ledger, err := timesheet.ReadDayArtifacts(statePath, day)
	if err != nil {
		return nil, "", err
	}
	if ledger.Entries == nil {
		return nil, "", fmt.Errorf("candidates: the day %s has no entries ledger to classify", day)
	}
	blocks := stagedBlocksFrom(ledgerUnits(ledger.Entries.Entries, day), ledger.StageMap)
	return blocks, timesheet.StagedLedgerDigest(day, blocks), nil
}

// prefetchCandidates reads the catalog for the day's staged blocks and writes
// candidates.json. A failed catalog read fails the whole step closed: no
// candidates are invented and no partial file is written.
func prefetchCandidates(h *headlessSession, cfg *timesheet.Config, day string) (candidatesFile, string, error) {
	blocks, digest, err := stagedDayBlocks(cfg.StatePath, day)
	if err != nil {
		return candidatesFile{}, "", err
	}
	out := candidatesFile{
		LedgerDigest: digest,
		Sets:         []nextCandidateSet{},
		Intervals:    map[string]string{},
	}
	picked := boundHistory(cfg.StatePath, day, digest)
	ids := make([]string, 0, len(blocks))
	for id := range blocks {
		if _, ok := picked[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	refs := map[string]string{}
	skipped := 0
	for _, id := range ids {
		q := candidateQuery(blocks[id])
		if q == "" {
			continue
		}
		if ref, ok := refs[q]; ok {
			out.Intervals[id] = ref
			continue
		}
		if len(out.Sets) >= maxCandidateQueries {
			skipped++
			continue
		}
		res, err := h.call("classifications", timesheet.Request{Query: q})
		if err != nil {
			return candidatesFile{}, "", err
		}
		receipt, ok := res.(timesheet.ClassificationsReceipt)
		if !ok {
			return candidatesFile{}, "", fmt.Errorf("candidates: unexpected classifications payload")
		}
		set := nextCandidateSet{
			Ref: fmt.Sprintf("c%d", len(out.Sets)+1), Query: q, CatalogID: receipt.CatalogID,
			Complete: receipt.Complete, Options: []nextCandidateOption{}, Note: receipt.Note,
		}
		for i, opt := range receipt.Options {
			if i == maxCandidateOptions {
				set.Complete = false
				set.Note = fmt.Sprintf("only the first %d options are shown; this list cannot prove an option absent", maxCandidateOptions)
				break
			}
			set.Options = append(set.Options, nextCandidateOption{ClassificationID: opt.ID, Label: opt.Name, Code: opt.Code})
		}
		out.Sets = append(out.Sets, set)
		refs[q] = set.Ref
		out.Intervals[id] = set.Ref
	}
	if skipped > 0 {
		out.Note = fmt.Sprintf("%d interval(s) beyond the first %d distinct topics carry no candidates", skipped, maxCandidateQueries)
	}
	dir, err := dayArtifactsDir(cfg.StatePath, day)
	if err != nil {
		return candidatesFile{}, "", err
	}
	if err := ensureDayArtifactsDir("candidates", dir); err != nil {
		return candidatesFile{}, "", err
	}
	path := filepath.Join(dir, "candidates.json")
	data, err := jsonBytes(out)
	if err != nil {
		return candidatesFile{}, "", err
	}
	if err := writeFileAtomic(path, data); err != nil {
		return candidatesFile{}, "", fmt.Errorf("candidates: %s cannot be written: %w", path, err)
	}
	return out, path, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func candidatesNotice(f candidatesFile) string {
	options := 0
	for _, s := range f.Sets {
		options += len(s.Options)
	}
	return fmt.Sprintf("%d catalog read(s), %d option(s) for %d interval(s)", len(f.Sets), options, len(f.Intervals))
}

func (d actDispatcher) candidatesStep(h *headlessSession) (string, error) {
	f, _, err := prefetchCandidates(h, d.cfg, d.day)
	if err != nil {
		return "", err
	}
	return candidatesNotice(f), nil
}

// candidatesReceipt is `classifications --candidates`'s stdout.
type candidatesReceipt struct {
	Date         string `json:"date"`
	File         string `json:"file"`
	LedgerDigest string `json:"ledger_digest"`
	Notice       string `json:"notice"`
}

// runCandidates is the verb form of the candidates step, so the derived argv
// is executable outside `next --do`.
func runCandidates(configPath, envFile, date string) error {
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return err
	}
	day, err := resolveDay(configPath, date)
	if err != nil {
		return err
	}
	h, err := openHeadless(configPath, envFile)
	if err != nil {
		return err
	}
	defer h.done()
	f, path, err := prefetchCandidates(h, cfg, day)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(candidatesReceipt{Date: day, File: path, LedgerDigest: f.LedgerDigest, Notice: candidatesNotice(f)})
}

// judgmentCandidates attaches each block's candidate ref and returns the sets
// the judgment embeds. Only sets some block references are returned.
func judgmentCandidates(f *candidatesFile, blocks []nextBlock) []nextCandidateSet {
	used := map[string]bool{}
	for i := range blocks {
		if ref, ok := f.Intervals[blocks[i].IntervalID]; ok {
			blocks[i].Candidates = ref
			used[ref] = true
		}
	}
	sets := make([]nextCandidateSet, 0, len(f.Sets))
	for _, s := range f.Sets {
		if used[s.Ref] {
			sets = append(sets, s)
		}
	}
	return sets
}

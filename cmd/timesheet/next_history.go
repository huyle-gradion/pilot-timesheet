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

// Priority, highest first: an explicit selection already in the sidecar; the
// configured default (gradion.default_classification), which fills every
// unselected interval, so history runs only when no default is configured;
// history; the judgment.
const (
	historyFileName      = "history.json"
	maxHistoryCatalogIDs = 12
)

// historyKey is the match key of one staged block, from its description
// alone so past and current days derive it the same way: "repo:<repo>" when
// the inline "PRs:" list names exactly one repository, "title:<normalized>"
// when it names none, and "" (never auto-selected) when it names several.
func historyKey(description string) string {
	title, prs := description, ""
	if i := strings.Index(description, "PRs:"); i >= 0 {
		title, prs = description[:i], description[i+len("PRs:"):]
	}
	repos := map[string]bool{}
	for _, seg := range strings.Split(prs, ";") {
		if repo, _, ok := strings.Cut(strings.TrimSpace(seg), ": "); ok && repo != "" {
			repos[repo] = true
		}
	}
	switch len(repos) {
	case 0:
		t := strings.ToLower(strings.Join(strings.Fields(title), " "))
		t = strings.TrimRight(t, " .;,-")
		if t == "" {
			return ""
		}
		return "title:" + t
	case 1:
		for repo := range repos {
			return "repo:" + repo
		}
	}
	return ""
}

type historyPick struct {
	CatalogID        string `json:"catalog_id"`
	ClassificationID string `json:"classification_id"`
	Key              string `json:"key"`
}

// historyFile is history.json in the day folder, bound to the staged ledger.
// It is written even when empty, so the step runs once per ledger.
type historyFile struct {
	LedgerDigest string                 `json:"ledger_digest"`
	Selections   map[string]historyPick `json:"selections"`
	Skipped      map[string]string      `json:"skipped,omitempty"`
}

func readHistoryFile(path string) (*historyFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f historyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return &historyFile{}, nil
	}
	return &f, nil
}

func boundHistory(statePath, day, digest string) map[string]historyPick {
	dir, err := dayArtifactsDir(statePath, day)
	if err != nil {
		return nil
	}
	f, err := readHistoryFile(filepath.Join(dir, historyFileName))
	if err != nil || f == nil || f.LedgerDigest != digest {
		return nil
	}
	return f.Selections
}

type historyVote struct {
	ids    map[string]bool
	choice timesheet.SubmittedSelection
}

// buildHistory writes history.json, and the bound choices answer when history
// classifies every interval. A failed catalog read fails the step closed.
func buildHistory(h *headlessSession, cfg *timesheet.Config, day string) (historyFile, error) {
	blocks, digest, err := stagedDayBlocks(cfg.StatePath, day)
	if err != nil {
		return historyFile{}, err
	}
	out := historyFile{LedgerDigest: digest, Selections: map[string]historyPick{}, Skipped: map[string]string{}}

	votes := map[string]*historyVote{}
	pastBlocks := map[string]map[string]timesheet.EntryRecord{}
	for _, sub := range h.session.SubmittedSelections(day) {
		past, ok := pastBlocks[sub.Date]
		if !ok {
			past, _, _ = stagedDayBlocks(cfg.StatePath, sub.Date)
			pastBlocks[sub.Date] = past
		}
		e, ok := past[sub.IntervalID]
		if !ok {
			continue
		}
		key := historyKey(e.Description)
		if key == "" {
			continue
		}
		v := votes[key]
		if v == nil {
			v = &historyVote{ids: map[string]bool{}, choice: sub}
			votes[key] = v
		}
		v.ids[sub.ClassificationID] = true
	}

	ids := make([]string, 0, len(blocks))
	for id := range blocks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	live := map[string]string{} // classification id -> live catalog id ("" = not found)
	for _, id := range ids {
		key := historyKey(blocks[id].Description)
		v := votes[key]
		switch {
		case key == "" || v == nil:
			continue
		case len(v.ids) > 1:
			out.Skipped[id] = "ambiguous: " + key + " was classified differently on different days"
			continue
		}
		cid := v.choice.ClassificationID
		catalogID, checked := live[cid]
		if !checked {
			if len(live) >= maxHistoryCatalogIDs {
				out.Skipped[id] = "beyond the per-day bound of catalog checks"
				continue
			}
			catalogID, err = liveCatalogFor(h, v.choice)
			if err != nil {
				return historyFile{}, err
			}
			live[cid] = catalogID
		}
		if catalogID == "" {
			out.Skipped[id] = "classification " + cid + " is not in the current live catalog"
			continue
		}
		out.Selections[id] = historyPick{CatalogID: catalogID, ClassificationID: cid, Key: key}
	}

	dir, err := dayArtifactsDir(cfg.StatePath, day)
	if err != nil {
		return historyFile{}, err
	}
	if err := ensureDayArtifactsDir("classify", dir); err != nil {
		return historyFile{}, err
	}
	if len(blocks) > 0 && len(out.Selections) == len(blocks) {
		if err := writeJSONFile(filepath.Join(dir, "choices.json"), historyAnswer(out)); err != nil {
			return historyFile{}, err
		}
	}
	if err := writeJSONFile(filepath.Join(dir, historyFileName), out); err != nil {
		return historyFile{}, err
	}
	return out, nil
}

// liveCatalogFor proves the submitted classification is still offered by
// reading the live catalog for its name (or code). It returns the catalog id
// that holds it, or "" when the read does not return it.
func liveCatalogFor(h *headlessSession, sub timesheet.SubmittedSelection) (string, error) {
	query := sub.Name
	if query == "" {
		query = sub.Code
	}
	if query == "" {
		return "", nil
	}
	res, err := h.call("classifications", timesheet.Request{Query: query})
	if err != nil {
		return "", err
	}
	receipt, ok := res.(timesheet.ClassificationsReceipt)
	if !ok {
		return "", fmt.Errorf("classify: unexpected classifications payload")
	}
	for _, o := range receipt.Options {
		if o.ID == sub.ClassificationID {
			return receipt.CatalogID, nil
		}
	}
	return "", nil
}

func historyAnswer(f historyFile) map[string]any {
	sel := map[string]timesheet.ClassificationChoiceEntry{}
	for id, p := range f.Selections {
		sel[id] = timesheet.ClassificationChoiceEntry{
			CatalogID: p.CatalogID, ClassificationID: p.ClassificationID, Source: timesheet.SelectionSourceHistory,
		}
	}
	return map[string]any{"ledger_digest": f.LedgerDigest, "selections": sel}
}

func writeJSONFile(path string, v any) error {
	data, err := jsonBytes(v)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, data); err != nil {
		return fmt.Errorf("classify: %s cannot be written: %w", path, err)
	}
	return nil
}

func mergeHistorySelections(statePath, day string, answer []byte, selections map[string]timesheet.ClassificationChoiceEntry) {
	for id, sel := range selections {
		if sel.Source == "" {
			sel.Source = timesheet.SelectionSourceJudgment
			selections[id] = sel
		}
	}
	var binding struct {
		LedgerDigest string `json:"ledger_digest"`
	}
	if json.Unmarshal(answer, &binding) != nil || binding.LedgerDigest == "" {
		return
	}
	for id, p := range boundHistory(statePath, day, binding.LedgerDigest) {
		if _, named := selections[id]; !named {
			selections[id] = timesheet.ClassificationChoiceEntry{
				CatalogID: p.CatalogID, ClassificationID: p.ClassificationID, Source: timesheet.SelectionSourceHistory,
			}
		}
	}
}

type statusClassification struct {
	IntervalID       string `json:"interval_id"`
	ClassificationID string `json:"classification_id"`
	Source           string `json:"source"`
}

func statusClassifications(choices map[string]timesheet.ClassificationChoiceEntry) []statusClassification {
	out := make([]statusClassification, 0, len(choices))
	for id, c := range choices {
		src := c.Source
		if src == "" {
			src = "unrecorded"
		}
		out = append(out, statusClassification{IntervalID: id, ClassificationID: c.ClassificationID, Source: src})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IntervalID < out[j].IntervalID })
	return out
}

func historyNotice(f historyFile) string {
	return fmt.Sprintf("%d interval(s) classified from history, %d skipped", len(f.Selections), len(f.Skipped))
}

func (d actDispatcher) classifyHistoryStep(h *headlessSession) (string, error) {
	f, err := buildHistory(h, d.cfg, d.day)
	if err != nil {
		return "", err
	}
	return historyNotice(f), nil
}

type historyReceipt struct {
	Date       string                 `json:"date"`
	File       string                 `json:"file"`
	Selections map[string]historyPick `json:"selections"`
	Skipped    map[string]string      `json:"skipped,omitempty"`
	Notice     string                 `json:"notice"`
}

func runClassifyHistory(f *finalizeFlags) error {
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	out, err := buildHistory(h, cfg, f.date)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	dir, err := dayArtifactsDir(cfg.StatePath, f.date)
	if err != nil {
		return err
	}
	return printJSON(historyReceipt{
		Date: f.date, File: filepath.Join(dir, historyFileName),
		Selections: out.Selections, Skipped: out.Skipped, Notice: historyNotice(out),
	})
}

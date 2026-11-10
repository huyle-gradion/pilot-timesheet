package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

//go:embed prompts/*.tmpl
var promptFS embed.FS

// Context bounds: long evidence is cut before it enters a prompt.
const (
	// MaxTitleRunes bounds a block title or an interval description.
	MaxTitleRunes = 300
	// MaxTopics and MaxTopicRunes bound the topic candidates per block.
	MaxTopics     = 8
	MaxTopicRunes = 160
	// MaxPRs bounds the pull-request labels listed per block.
	MaxPRs = 12
	// maxDescriptionRunes is the ledger's description limit (prose plus the
	// appended PRs list).
	maxDescriptionRunes = 500
	minProseRunes       = 60
)

// ErrInvalidReply marks a reply that does not validate against the judgment.
var ErrInvalidReply = errors.New("the judge reply is not a valid answer")

// Prompt is one rendered prompt plus the cache key that identifies it.
type Prompt struct {
	Text string
	// Key binds the template bytes, the backend identity and the rendered
	// context: any change to one of them misses the cache.
	Key string
}

func render(name string, data map[string]any, backend Backend) (Prompt, error) {
	raw, err := promptFS.ReadFile("prompts/" + name + ".tmpl")
	if err != nil {
		return Prompt{}, err
	}
	tmpl, err := template.New(name).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return Prompt{}, err
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, data); err != nil {
		return Prompt{}, err
	}
	sum := sha256.New()
	for _, part := range [][]byte{raw, []byte(backend.Identity()), b.Bytes()} {
		sum.Write(part)
		sum.Write([]byte{0})
	}
	return Prompt{Text: b.String(), Key: fmt.Sprintf("%x", sum.Sum(nil))}, nil
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func compactJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// --- descriptions -----------------------------------------------------------

type describeBlock struct {
	BlockID          string   `json:"block_id"`
	Kind             string   `json:"kind"`
	Title            string   `json:"title,omitempty"`
	Topics           []string `json:"topics,omitempty"`
	PRs              []string `json:"prs,omitempty"`
	OutsideWorkHours bool     `json:"outside_work_hours,omitempty"`
}

// proseLimit is how long a block's prose may be so that record's appended
// PRs list still fits the ledger's description limit.
func proseLimit(b timesheet.DraftBlock) int {
	n := maxDescriptionRunes
	if b.PRsInline != "" {
		n -= len([]rune(b.PRsInline)) + 1
	}
	if n < minProseRunes {
		n = minProseRunes
	}
	return n
}

// DescribePrompt renders the descriptions prompt for a draft.
func DescribePrompt(d timesheet.Draft, backend Backend) (Prompt, error) {
	blocks := make([]describeBlock, 0, len(d.Blocks))
	limit := maxDescriptionRunes
	for _, b := range d.Blocks {
		db := describeBlock{BlockID: b.BlockID, Kind: b.Kind, Title: clip(b.Title, MaxTitleRunes), OutsideWorkHours: b.OutsideWorkHours}
		for i, t := range b.TopicCandidates {
			if i == MaxTopics {
				break
			}
			db.Topics = append(db.Topics, clip(t, MaxTopicRunes))
		}
		for i, pr := range b.PRs {
			if i == MaxPRs {
				break
			}
			db.PRs = append(db.PRs, clip(pr, MaxTopicRunes))
		}
		blocks = append(blocks, db)
		if l := proseLimit(b); l < limit {
			limit = l
		}
	}
	return render("descriptions", map[string]any{"Input": compactJSON(blocks), "MaxRunes": limit}, backend)
}

// ParseDescriptions validates a reply against the draft: every block answered
// with non-empty prose, no unknown ids. Over-long prose is cut so the PRs
// list record appends still fits.
func ParseDescriptions(d timesheet.Draft, reply string) (timesheet.DescriptionsFile, error) {
	var out struct {
		Notes        string            `json:"notes"`
		Descriptions map[string]string `json:"descriptions"`
	}
	if err := decodeObject(reply, &out); err != nil {
		return timesheet.DescriptionsFile{}, err
	}
	known := map[string]timesheet.DraftBlock{}
	for _, b := range d.Blocks {
		known[b.BlockID] = b
	}
	var missing, unknown []string
	for id := range out.Descriptions {
		if _, ok := known[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	for _, b := range d.Blocks {
		if strings.TrimSpace(out.Descriptions[b.BlockID]) == "" {
			missing = append(missing, b.BlockID)
		}
	}
	sort.Strings(unknown)
	switch {
	case len(missing) > 0:
		return timesheet.DescriptionsFile{}, fmt.Errorf("%w: no description for block(s) %s", ErrInvalidReply, strings.Join(missing, ", "))
	case len(unknown) > 0:
		return timesheet.DescriptionsFile{}, fmt.Errorf("%w: unknown block id(s) %s", ErrInvalidReply, strings.Join(unknown, ", "))
	}
	f := timesheet.DescriptionsFile{Notes: clip(out.Notes, MaxTitleRunes), Descriptions: map[string]string{}}
	for id, text := range out.Descriptions {
		f.Descriptions[id] = clip(text, proseLimit(known[id]))
	}
	return f, nil
}

// --- classifications --------------------------------------------------------

// Option is one catalog option a set offers.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Code  string `json:"code,omitempty"`
}

// CandidateSet is one prefetched catalog read (issue #234).
type CandidateSet struct {
	Ref       string   `json:"-"`
	CatalogID string   `json:"-"`
	Options   []Option `json:"options"`
}

// Interval is one staged interval to classify; Set names its candidate set
// ("" when it has none).
type Interval struct {
	IntervalID  string `json:"interval_id"`
	Start       string `json:"start,omitempty"`
	End         string `json:"end,omitempty"`
	Description string `json:"description"`
	Set         string `json:"set,omitempty"`
}

// Selection is one validated pick, with the catalog id the binary fills in.
type Selection struct {
	CatalogID        string
	ClassificationID string
}

// ClassifyPrompt renders the classifications prompt.
func ClassifyPrompt(intervals []Interval, sets []CandidateSet, backend Backend) (Prompt, error) {
	in := make([]Interval, 0, len(intervals))
	for _, iv := range intervals {
		iv.Description = clip(iv.Description, MaxTitleRunes)
		in = append(in, iv)
	}
	bySet := map[string][]Option{}
	for _, s := range sets {
		bySet[s.Ref] = s.Options
	}
	return render("classifications", map[string]any{"Input": compactJSON(in), "Sets": compactJSON(bySet)}, backend)
}

// ParseSelections validates a reply: every pick names a listed interval and an
// option from that interval's own set. Anything else is an invalid reply.
func ParseSelections(intervals []Interval, sets []CandidateSet, reply string) (map[string]Selection, error) {
	var out struct {
		Selections map[string]string `json:"selections"`
	}
	if err := decodeObject(reply, &out); err != nil {
		return nil, err
	}
	setOf := map[string]string{}
	for _, iv := range intervals {
		setOf[iv.IntervalID] = iv.Set
	}
	byRef := map[string]CandidateSet{}
	for _, s := range sets {
		byRef[s.Ref] = s
	}
	picks := map[string]Selection{}
	for _, id := range sortedKeys(out.Selections) {
		ref, ok := setOf[id]
		if !ok {
			return nil, fmt.Errorf("%w: unknown interval id %s", ErrInvalidReply, id)
		}
		set, ok := byRef[ref]
		if !ok || ref == "" {
			return nil, fmt.Errorf("%w: interval %s has no candidate set", ErrInvalidReply, id)
		}
		found := false
		for _, o := range set.Options {
			if o.ID == out.Selections[id] {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: interval %s: %q is not an option of its set", ErrInvalidReply, id, out.Selections[id])
		}
		picks[id] = Selection{CatalogID: set.CatalogID, ClassificationID: out.Selections[id]}
	}
	return picks, nil
}

// --- shared -----------------------------------------------------------------

// decodeObject reads the one JSON object in a reply, tolerating a code fence
// or surrounding text a harness may add.
func decodeObject(reply string, v any) error {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return fmt.Errorf("%w: no JSON object in the reply", ErrInvalidReply)
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), v); err != nil {
		return fmt.Errorf("%w: the reply is not valid JSON for the answer schema", ErrInvalidReply)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Ask sends a prompt and validates the reply with parse. A backend failure or
// an invalid reply is retried once, with a one-line note of what went wrong;
// a second failure is returned. Usage covers every call made.
func Ask[T any](ctx context.Context, backend Backend, p Prompt, parse func(string) (T, error)) (T, string, Usage, error) {
	var zero T
	var total Usage
	total.Exact = true
	prompt := p.Text
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		reply, usage, err := backend.Complete(ctx, prompt)
		total = total.add(usage)
		if err == nil {
			var v T
			if v, err = parse(reply); err == nil {
				return v, reply, total, nil
			}
		}
		lastErr = err
		prompt = p.Text + "\n\nYour previous reply was rejected: " + clip(err.Error(), 300) + "\nReturn only the JSON object."
	}
	return zero, "", total, lastErr
}

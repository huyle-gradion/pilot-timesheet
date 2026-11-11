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
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

//go:embed prompts/*.tmpl
var promptFS embed.FS

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
	// prsSeparatorRunes is the space that joins the prose to the PRs list.
	prsSeparatorRunes = 1
)

// ErrInvalidReply marks a reply that does not validate against the judgment.
var ErrInvalidReply = errors.New("the judge reply is not a valid answer")

type Prompt struct {
	Text string
	Key  string
}

func render(name string, data map[string]any, backend Backend) (Prompt, error) {
	raw, err := promptFS.ReadFile("prompts/" + name + ".tmpl")
	if err != nil {
		return Prompt{}, err
	}
	return renderTemplate(name, raw, data, backend)
}

func renderTemplate(name string, raw []byte, data map[string]any, backend Backend) (Prompt, error) {
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

type describeBlock struct {
	BlockID          string   `json:"block_id"`
	Kind             string   `json:"kind"`
	Title            string   `json:"title,omitempty"`
	Topics           []string `json:"topics,omitempty"`
	PRs              []string `json:"prs,omitempty"`
	OutsideWorkHours bool     `json:"outside_work_hours,omitempty"`
}

func proseLimit(b timesheet.DraftBlock) int {
	n := maxDescriptionRunes
	if b.PRsInline != "" {
		n -= len([]rune(b.PRsInline)) + prsSeparatorRunes
	}
	if n < minProseRunes {
		n = minProseRunes
	}
	return n
}

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

type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Code  string `json:"code,omitempty"`
}

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

type Selection struct {
	CatalogID        string
	ClassificationID string
}

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

const (
	// RetryWait is the shortest pause before a transient failure is retried.
	RetryWait = time.Second
	// MaxRetryWait bounds that pause: a provider asking for longer gets no
	// retry, since an earlier one would only spend another call.
	MaxRetryWait = 10 * time.Second
)

// RetryPolicy shapes Ask's single retry. BeforeCall runs before every call
// with the exact prompt and the usage Ask has spent so far; its error stops
// Ask without that call. Wait pauses before a transient retry; nil sleeps
// until the pause ends or ctx is done.
type RetryPolicy struct {
	BeforeCall func(prompt string, spent Usage) error
	Wait       func(ctx context.Context, d time.Duration) error
}

// Ask sends a prompt and validates the reply with parse. An invalid reply is
// retried once with a one-line note of what was wrong. A transient backend
// failure is retried once, unchanged, after RetryWait or the provider's
// Retry-After when longer (up to MaxRetryWait). Anything else, and a second
// failure, is returned. When BeforeCall stops the retry, its error keeps the
// first call's failure. Usage covers every call made.
func Ask[T any](ctx context.Context, backend Backend, p Prompt, parse func(string) (T, error), policy RetryPolicy) (T, string, Usage, error) {
	var zero T
	total := Usage{Exact: true}
	prompt := p.Text
	var prior error
	for attempt := 0; ; attempt++ {
		if policy.BeforeCall != nil {
			if err := policy.BeforeCall(prompt, total); err != nil {
				if prior != nil {
					err = fmt.Errorf("%w; the first call failed: %w", err, prior)
				}
				return zero, "", total, err
			}
		}
		reply, usage, err := backend.Complete(ctx, prompt)
		if usage.Calls == 0 {
			usage.Calls = 1
		}
		total = total.add(usage)
		if err == nil {
			v, parseErr := parse(reply)
			if parseErr == nil {
				return v, reply, total, nil
			}
			err = parseErr
		}
		if attempt > 0 {
			return zero, "", total, err
		}
		prior = err
		if errors.Is(err, ErrInvalidReply) {
			prompt = p.Text + "\n\nYour previous reply was rejected: " + clip(err.Error(), 300) + "\nReturn only the JSON object."
			continue
		}
		d, ok := retryDelay(err)
		if !ok {
			return zero, "", total, err
		}
		wait := policy.Wait
		if wait == nil {
			wait = sleep
		}
		if wait(ctx, d) != nil {
			return zero, "", total, err
		}
	}
}

func retryDelay(err error) (time.Duration, bool) {
	var be *BackendError
	if !errors.As(err, &be) || !be.Transient {
		return 0, false
	}
	d := max(RetryWait, be.RetryAfter)
	return d, d <= MaxRetryWait
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

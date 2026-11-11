package timesheet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The headless surface is the deterministic [Script] layer for the timesheet
// skill: BuildDraft turns one day's collected review into the compact draft the
// skill's single judgment step reviews, and RecordEntries persists the reviewed
// result as the day's log entries. Neither function performs collection,
// confirmation, approval, submission, or any network access.

type DraftSourceItem struct {
	Kind  string `json:"kind"`
	Repo  string `json:"repo,omitempty"`
	Title string `json:"title,omitempty"`
	Time  string `json:"time,omitempty"`
	URL   string `json:"url,omitempty"`
}

// DraftBlock is one time block of the day: a calendar meeting, a confirmed
// work interval, a stretch of evidence-derived work, or — when the day has no
// time anchors at all — one whole-day block. Blocks never overlap: a meeting
// keeps its calendar span, confirmed work keeps its own, and evidence claims
// only the minutes neither of those owns. A meeting still names the proof of
// work that happened inside it through its topic candidates.
//
// The draft is the AI-visible representation only. Audit-only fields stay out
// of it: Sources and SourcesTruncated are kept in the Go struct for the record
// path but never serialize into the draft JSON, and the full per-item evidence
// (titles, URLs) remains durable in the store's day evidence regardless.
type DraftBlock struct {
	BlockID         string   `json:"block_id"`
	Kind            string   `json:"kind"` // meeting|work|evidence|day
	Start           string   `json:"start,omitempty"`
	End             string   `json:"end,omitempty"`
	Title           string   `json:"title,omitempty"`
	TopicCandidates []string `json:"topic_candidates,omitempty"`
	PRs             []string `json:"prs,omitempty"`
	// PRsInline is the exact "PRs: ..." description suffix rendered script-side
	// from the block's PR labels in stable numeric order. When present, the
	// judgment step copies it verbatim into the block description; record
	// validates only that a block with PRs carries a "PRs:" list — prs_inline
	// is the exact string the script supplies for it.
	PRsInline        string            `json:"prs_inline,omitempty"`
	Sources          []DraftSourceItem `json:"-"`
	SourcesTruncated bool              `json:"-"`
	Completeness     map[string]string `json:"completeness,omitempty"`
	// OutsideWorkHours and WorkHoursReason are presentation-only annotations
	// added by AnnotateWorkHours from the optional work_hours policy. They
	// never reject, warn, or change a block's span or duration.
	OutsideWorkHours bool   `json:"outside_work_hours,omitempty"`
	WorkHoursReason  string `json:"work_hours_reason,omitempty"`
}

// Draft is the entire input of the skill's judgment step. It carries one day
// only, never raw provider payloads, and never credentials. Its size is
// bounded on mechanical days by fixed rendering rules: topic titles collapse
// per the compactTopics rules and topic_candidates is capped at
// maxDraftTopicCandidates with an honest overflow line. Durable state and
// recorded entries keep full fidelity; only this output is compacted.
type Draft struct {
	Date       string       `json:"date"`
	Timezone   string       `json:"timezone"`
	AppVersion string       `json:"app_version"`
	DayNote    string       `json:"day_note,omitempty"`
	Flags      []string     `json:"flags,omitempty"`
	Blocks     []DraftBlock `json:"blocks"`
	// WorkHours echoes the policy that produced the block annotations, so a
	// reader can check an outside_work_hours claim against the policy itself
	// instead of trusting the reason token alone. It is omitted entirely when
	// no policy is configured, keeping that draft byte-identical.
	WorkHours *WorkHoursConfig `json:"work_hours,omitempty"`
}

const draftSourcesPerBlock = 8

// BuildDraft applies the fixed, documented partitioning rule to one review and
// renders the compact one-day draft. The partition itself is partitionDay, so
// record recomputes the same blocks and sources from the same durable inputs.
func BuildDraft(review Review, appVersion string) Draft {
	d := Draft{
		Date:       review.Date,
		Timezone:   review.Timezone,
		AppVersion: appVersion,
	}
	var completeness map[string]string
	if review.Evidence != nil {
		completeness = map[string]string{}
		for _, src := range review.Evidence.Sources {
			if !src.Enabled {
				continue
			}
			completeness[src.Source] = src.Status
			if src.Status != StatusComplete && !src.Acknowledged {
				d.Flags = append(d.Flags, "source_incomplete:"+src.Source)
			}
		}
	}
	if len(review.Confirmed.Segments) == 0 {
		d.Flags = append(d.Flags, "no_confirmed_intervals")
	}

	var topics []TopicCandidate
	var meetings []ProposedMeeting
	if review.Evidence != nil {
		topics = review.Evidence.Topics
		meetings = review.Evidence.Meetings
	}
	partition := partitionDay(topics, meetings, review.Confirmed.Segments, review.Timezone)
	if partition.EligibleMeetings == 0 {
		d.DayNote = "no calendar events"
	}
	for i := range partition.Blocks {
		blk := partition.Blocks[i].blk
		blk.Completeness = completeness
		sortPRs(blk.PRs)
		blk.TopicCandidates = compactTopicsRender(blk.TopicCandidates, partition.Blocks[i].overflow)
		blk.PRsInline = prsInline(blk.PRs)
		d.Blocks = append(d.Blocks, blk)
	}
	return d
}

// Work-hours annotation reasons. These tokens are contractual: they appear in
// the draft JSON and are the only values WorkHoursReason ever carries.
const (
	workReasonDayNotInPolicy = "day_not_in_policy"
	workReasonBeforeWindow   = "before_window"
	workReasonBetweenWindows = "between_windows"
	workReasonAfterWindow    = "after_window"
	workReasonPartial        = "partial"
)

type workWindowMinutes struct {
	start int
	end   int
}

// AnnotateWorkHours marks each draft block that is fully or partly outside the
// optional main-work-time policy. It is presentation only: nothing is
// rejected, warned, or removed, and no span or duration changes. A nil draft or
// nil policy is a no-op, as is a draft whose day cannot be read or a block with
// no usable same-day start/end (for example the whole-day fallback block).
//
// A block entirely inside one window is left untouched. A block that starts
// before the day's first window, sits in a gap between windows, or extends past
// the last window is marked with the matching reason; a block that overlaps a
// window boundary (partly inside, partly outside) is marked partial. A day not
// named by the policy marks every usable block day_not_in_policy.
func AnnotateWorkHours(d *Draft, policy *WorkHoursConfig, loc *time.Location) {
	if d == nil || policy == nil {
		return
	}
	if loc == nil {
		// No recorded-day timezone means no honest classification: a
		// substitute zone would guess at the weekday and the window clock.
		return
	}
	day, err := time.ParseInLocation("2006-01-02", d.Date, loc)
	if err != nil {
		return
	}
	// Echo the policy only once it is actually applied, so every reason token
	// below is checkable against the policy that produced it.
	d.WorkHours = policy
	allowed := make(map[string]bool, len(policy.Days))
	for _, name := range policy.Days {
		allowed[name] = true
	}
	inPolicy := allowed[weekdayToken(day.Weekday())]

	windows := make([]workWindowMinutes, 0, len(policy.Windows))
	for _, win := range policy.Windows {
		start, errStart := parseClock(win.Start)
		end, errEnd := parseClock(win.End)
		if errStart != nil || errEnd != nil || start >= end {
			// A policy reaching this function is validated config; an invalid
			// window is skipped rather than guessed at.
			continue
		}
		windows = append(windows, workWindowMinutes{start: start, end: end})
	}
	if inPolicy && len(windows) == 0 {
		return
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].start < windows[j].start })

	for i := range d.Blocks {
		b := &d.Blocks[i]
		start, errStart := parseClock(b.Start)
		end, errEnd := parseClock(b.End)
		if errStart != nil || errEnd != nil || end <= start {
			continue
		}
		b.OutsideWorkHours = false
		b.WorkHoursReason = ""
		if !inPolicy {
			b.OutsideWorkHours = true
			b.WorkHoursReason = workReasonDayNotInPolicy
			continue
		}
		reason := workHoursReasonFor(start, end, windows)
		if reason == "" {
			continue
		}
		b.OutsideWorkHours = true
		b.WorkHoursReason = reason
	}
}

// workHoursReasonFor classifies one [start,end) block against sorted, disjoint
// windows. It returns "" when the block is entirely inside one window.
func workHoursReasonFor(start, end int, windows []workWindowMinutes) string {
	for _, w := range windows {
		if start >= w.start && end <= w.end {
			return ""
		}
	}
	for _, w := range windows {
		if start < w.end && end > w.start {
			return workReasonPartial
		}
	}
	if end <= windows[0].start {
		return workReasonBeforeWindow
	}
	if start >= windows[len(windows)-1].end {
		return workReasonAfterWindow
	}
	return workReasonBetweenWindows
}

// weekdayToken renders Go's weekday as the exact three-letter token the
// work_hours policy uses.
func weekdayToken(w time.Weekday) string {
	return [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}[w]
}

// topicOverflow accumulates one topic's — or one rendered group's — durable
// overflow counts for the AI-visible draft (issue #146): the count of items
// retained durably beyond the per-topic draft cap by item kind, the count
// lost beyond the fixed overflow ceilings, and the union span of the
// overflow items' timestamps as local minutes. Grouping combines topics by
// summation, so a grouped/mechanical rendered line carries ONE combined
// summary and the "+N more" line can carry the aggregate of the omitted
// lines' losses.
type topicOverflow struct {
	kinds      map[string]int
	kept       int
	lost       int
	minM, maxM int
}

// topicOverflowOf collects one topic's overflow counts. It returns nil for a
// topic with nothing beyond the retained Items cap, so non-truncated topics
// contribute nothing to any rendered line.
func topicOverflowOf(t TopicCandidate, loc *time.Location) *topicOverflow {
	if len(t.OverflowItems) == 0 && t.DroppedItems == 0 {
		return nil
	}
	o := &topicOverflow{kinds: map[string]int{}, minM: -1, maxM: -1}
	for _, item := range t.OverflowItems {
		k := item.Kind
		if k == "" {
			k = "unclassified"
		}
		o.kinds[k]++
		o.kept++
		if m, ok := anchorClock(item.Time, loc); ok {
			if o.minM < 0 || m < o.minM {
				o.minM = m
			}
			if o.maxM < 0 || m > o.maxM {
				o.maxM = m
			}
		}
	}
	o.lost = t.DroppedItems
	return o
}

// merge folds another overflow accumulator into this one (nil is a no-op).
func (o *topicOverflow) merge(p *topicOverflow) {
	if p == nil || o == nil {
		return
	}
	for k, n := range p.kinds {
		o.kinds[k] += n
	}
	o.kept += p.kept
	o.lost += p.lost
	if p.minM >= 0 {
		if o.minM < 0 || p.minM < o.minM {
			o.minM = p.minM
		}
		if o.maxM < 0 || p.maxM > o.maxM {
			o.maxM = p.maxM
		}
	}
}

func (o *topicOverflow) empty() bool {
	return o == nil || (o.kept == 0 && o.lost == 0)
}

// suffix renders the deterministic overflow summary for one line: the count
// of retained items the line does not list ("+N more items not listed")
// with per-kind counts in alphabetical order, the honest lost count,
// and the first/last overflow timestamps as local clock times (computed over
// the overflow items only — a topic's FirstAt/LastAt span is unchanged by
// overflow retention — unparseable timestamps are skipped). Empty for a
// group with nothing beyond the retained Items cap.
func (o *topicOverflow) suffix() string {
	if o.empty() {
		return ""
	}
	var parts []string
	if o.kept > 0 {
		names := make([]string, 0, len(o.kinds))
		for k := range o.kinds {
			names = append(names, k)
		}
		sort.Strings(names)
		counts := make([]string, 0, len(names))
		for _, k := range names {
			counts = append(counts, fmt.Sprintf("%s %d", k, o.kinds[k]))
		}
		parts = append(parts, fmt.Sprintf("+%d more items not listed (%s)", o.kept, strings.Join(counts, ", ")))
	}
	if o.lost > 0 {
		parts = append(parts, fmt.Sprintf("%d lost", o.lost))
	}
	if o.minM >= 0 {
		parts = append(parts, clockString(o.minM)+"–"+clockString(o.maxM))
	}
	return " — overflow: " + strings.Join(parts, ", ")
}

func draftOverflowSummary(t TopicCandidate) string {
	return topicOverflowOf(t, nil).suffix()
}

// prLabel renders "pr:owner/name:12" as "#12 owner/name".
func prLabel(resource string) string {
	parts := strings.Split(resource, ":")
	if len(parts) != 3 || parts[2] == "" {
		return ""
	}
	return "#" + parts[2] + " " + parts[1]
}

// sortPRs orders PR labels by their numeric PR number, then by label text, so
// the draft's prs and prs_inline order is stable and human-numeric ("#2"
// before "#10").
func sortPRs(prs []string) {
	sort.SliceStable(prs, func(i, j int) bool {
		ni, oki := prLabelNumber(prs[i])
		nj, okj := prLabelNumber(prs[j])
		if oki && okj && ni != nj {
			return ni < nj
		}
		return prs[i] < prs[j]
	})
}

func prLabelNumber(label string) (int, bool) {
	if len(label) == 0 || label[0] != '#' {
		return 0, false
	}
	rest := label[1:]
	if end := strings.IndexByte(rest, ' '); end >= 0 {
		rest = rest[:end]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// splitPRLabel splits a "#N repo" PR label into its repository and numeric PR
// number. It reports ok=false for anything else (no "#N" prefix, a
// non-numeric number, or an empty repository); prsInline renders such labels
// verbatim as deterministically sorted trailing entries.
func splitPRLabel(label string) (repo string, num int, ok bool) {
	if len(label) == 0 || label[0] != '#' {
		return "", 0, false
	}
	rest := label[1:]
	end := strings.IndexByte(rest, ' ')
	if end < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return "", 0, false
	}
	repo = rest[end+1:]
	if repo == "" {
		return "", 0, false
	}
	return repo, n, true
}

// prsInline renders the exact "PRs: ..." description suffix a block with PRs
// must carry, compacted so a many-PR block's composed interval description
// stays under the 500-rune description limit (issue PRS-COMPACT-1): one
// segment per repository, repositories in alphabetical order, each segment
// "<repo>: <numbers>" with the numbers rendered as contiguous numeric ranges
// ("#a–#b", a lone "#a", gaps comma-listed — the same rule as draftPRRanges)
// and segments joined by "; ". Every PR number appears exactly once, each
// repository is stated once per segment, and the same input always yields
// byte-identical output. Labels that do not parse as "#N repo" are appended
// verbatim, sorted, as trailing entries. The judgment step copies it verbatim
// into the description; record validates only the presence of a "PRs:" list
// in the description, and prs_inline is the exact string the script supplies
// for it, so rendering it script-side removes the dropped-list rejection
// mode. A block with no PRs has no prs_inline.
func prsInline(prs []string) string {
	if len(prs) == 0 {
		return ""
	}
	byRepo := map[string][]int{}
	var unparsed []string
	for _, label := range prs {
		if repo, num, ok := splitPRLabel(label); ok {
			byRepo[repo] = append(byRepo[repo], num)
		} else {
			unparsed = append(unparsed, label)
		}
	}
	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	segments := make([]string, 0, len(repos)+len(unparsed))
	for _, repo := range repos {
		segments = append(segments, repo+": "+draftPRRanges(byRepo[repo]))
	}
	if len(unparsed) > 0 {
		sort.Strings(unparsed)
		segments = append(segments, unparsed...)
	}
	return "PRs: " + strings.Join(segments, "; ")
}

// draftPRTitleRE matches the mechanical per-PR topic title the GitHub
// collector renders ("Pull request #12 (acme/api)"). The number and
// repository are the grouping identity; grouping never depends on free text.
var draftPRTitleRE = regexp.MustCompile(`^Pull request #(\d+) \(([^)]+)\)$`)

func draftPRTitle(title string) (repo string, num int, ok bool) {
	m := draftPRTitleRE.FindStringSubmatch(title)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return "", 0, false
	}
	return m[2], n, true
}

// draftPRRanges renders sorted, de-duplicated PR numbers as contiguous numeric
// ranges joined by ", ", each "#a–#b" or a lone "#a". Different repositories
// and non-adjacent numbers never merge into one range.
func draftPRRanges(nums []int) string {
	sorted := append([]int(nil), nums...)
	sort.Ints(sorted)
	parts := make([]string, 0, len(sorted))
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, fmt.Sprintf("#%d", sorted[i]))
		} else {
			parts = append(parts, fmt.Sprintf("#%d–#%d", sorted[i], sorted[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

func compactTopics(titles []string) []string {
	return compactTopicsRender(titles, nil)
}

// compactTopicsRender is compactTopics with an optional per-title overflow
// payload (issue #146). Grouping and capping see the bare titles exactly as
// #148 pinned them; the overflow summaries attach afterwards, per rendered
// line:
//
//   - A grouped line ("N PRs (#a–#b) in repo") or a collapsed repeat line
//     ("N× title") carries ONE combined summary for the topics it
//     aggregates: summed per-kind kept counts, summed lost count, and the
//     union first/last overflow span. A lone PR or verbatim distinct title
//     keeps its own per-topic summary.
//   - When the rendered list is capped, the omitted lines' overflow
//     aggregates onto the "+N more" line itself, so the headline loss count
//     is never hidden behind the cap.
//
// With all-nil payloads (or via compactTopics) the output is byte-identical
// to the #148 renderer.
func compactTopicsRender(titles []string, overflows []*topicOverflow) []string {
	ov := func(i int) *topicOverflow {
		if i < len(overflows) {
			return overflows[i]
		}
		return nil
	}
	type prGroup struct {
		nums []int
		seen map[int]bool
		sum  *topicOverflow
	}
	prGroups := map[string]*prGroup{}
	var prRepoOrder []string
	repeat := map[string]int{}
	repeatSum := map[string]*topicOverflow{}
	for i, t := range titles {
		if repo, num, ok := draftPRTitle(t); ok {
			g, seen := prGroups[repo]
			if !seen {
				g = &prGroup{seen: map[int]bool{}, sum: &topicOverflow{kinds: map[string]int{}, minM: -1, maxM: -1}}
				prGroups[repo] = g
				prRepoOrder = append(prRepoOrder, repo)
			}
			if !g.seen[num] {
				g.seen[num] = true
				g.nums = append(g.nums, num)
			}
			g.sum.merge(ov(i))
			continue
		}
		repeat[t]++
		if repeatSum[t] == nil {
			repeatSum[t] = &topicOverflow{kinds: map[string]int{}, minM: -1, maxM: -1}
		}
		repeatSum[t].merge(ov(i))
	}
	var out []string
	lineSums := []*topicOverflow(nil)
	renderedPR := map[string]bool{}
	renderedTitle := map[string]bool{}
	for i, t := range titles {
		if repo, _, ok := draftPRTitle(t); ok {
			if renderedPR[repo] {
				continue
			}
			renderedPR[repo] = true
			g := prGroups[repo]
			if len(g.nums) == 1 {
				out = append(out, t+g.sum.suffix())
				lineSums = append(lineSums, g.sum)
				continue
			}
			out = append(out, fmt.Sprintf("%d PRs (%s) in %s", len(g.nums), draftPRRanges(g.nums), repo)+g.sum.suffix())
			lineSums = append(lineSums, g.sum)
			continue
		}
		if renderedTitle[t] {
			continue
		}
		renderedTitle[t] = true
		if repeat[t] >= 2 {
			out = append(out, fmt.Sprintf("%d× %s", repeat[t], t)+repeatSum[t].suffix())
			lineSums = append(lineSums, repeatSum[t])
			continue
		}
		out = append(out, t+ov(i).suffix())
		lineSums = append(lineSums, ov(i))
	}
	if len(out) > maxDraftTopicCandidates {
		agg := &topicOverflow{kinds: map[string]int{}, minM: -1, maxM: -1}
		for _, s := range lineSums[maxDraftTopicCandidates:] {
			agg.merge(s)
		}
		more := fmt.Sprintf("+%d more", len(out)-maxDraftTopicCandidates) + agg.suffix()
		out = append(out[:maxDraftTopicCandidates:maxDraftTopicCandidates], more)
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TodayIn resolves the local date of one instant in a configured timezone.
// The daily driver calls this with time.Now so an omitted --date means today on
// the recorded day's clock, not the host clock: a UTC host and an
// Asia/Ho_Chi_Minh user disagree for seven hours every evening.
func TodayIn(timezone string, now time.Time) (string, error) {
	if strings.TrimSpace(timezone) == "" {
		return "", newError(CodeConfigInvalid, "config: timezone is required to resolve today")
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return "", newError(CodeConfigInvalid, "config: timezone is not a recognised IANA timezone")
	}
	return now.In(loc).Format("2006-01-02"), nil
}

// DayLocationOrNil resolves a recorded day's IANA timezone for anchor
// conversion. An empty or unloadable zone yields nil, which means "take the
// anchor at its own offset": empty time.LoadLocation("") would otherwise
// silently mean UTC and shift every anchor, so it is rejected here.
func DayLocationOrNil(timezone string) *time.Location {
	if strings.TrimSpace(timezone) == "" {
		return nil
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil
	}
	return loc
}

// anchorClock resolves one absolute provider anchor into minutes since local
// midnight on the recorded day. An RFC3339 instant is converted into loc first,
// because a provider stamp in its own offset (usually UTC) is not a local wall
// clock; a bare "HH:MM" is already local and is taken as written. A nil loc
// leaves the stamp on its own offset, which is reachable only when the recorded
// day's timezone is empty or cannot be loaded.
func anchorClock(s string, loc *time.Location) (int, bool) {
	s = strings.TrimSpace(s)
	if loc != nil && len(s) >= 16 && (s[10] == 'T' || s[10] == ' ') {
		if t, err := time.Parse(time.RFC3339, strings.Replace(s, " ", "T", 1)); err == nil {
			local := t.In(loc)
			return local.Hour()*60 + local.Minute(), true
		}
	}
	return draftClock(s)
}

// draftClock accepts the two time shapes the review carries — "HH:MM" for
// intervals and RFC 3339 for provider timestamps — and returns minutes on the
// stamp's own clock. Anchor conversion belongs to anchorClock.
func draftClock(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if t, ok := draftStamp(s); ok {
		return t.Hour()*60 + t.Minute(), true
	}
	if t, err := time.Parse("15:04", s); err == nil {
		return t.Hour()*60 + t.Minute(), true
	}
	return 0, false
}

// draftStamp parses the RFC 3339 shape draftClock accepts, keeping the date
// and offset the clock alone drops. An "HH:MM" anchor is not a stamp. It trims
// surrounding space exactly as draftClock always has, so a padded stamp parses
// here iff draftClock already read its clock; no anchor changes meaning.
func draftStamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 16 && (s[10] == 'T' || s[10] == ' ') {
		if t, err := time.Parse(time.RFC3339, strings.Replace(s, " ", "T", 1)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// minutesPerDay is one wall-clock day; the overlap projection adds it to an
// overnight end so the span runs past midnight on one timeline.
const minutesPerDay = 24 * 60

func clockString(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// ReviewedBlock is a draft block after the skill's judgment step. Description
// is the grouped topic summary; when PRs is non-empty, RecordEntries validates
// only that the description carries a "PRs:" list — prs_inline is the exact
// string the script renders for it.
type ReviewedBlock struct {
	DraftBlock
	Description string `json:"description"`
	Notes       string `json:"notes,omitempty"`
}

type ReviewedDraft struct {
	Date     string          `json:"date"`
	Timezone string          `json:"timezone"`
	DayNote  string          `json:"day_note,omitempty"`
	Flags    []string        `json:"flags,omitempty"`
	Notes    string          `json:"notes,omitempty"`
	Blocks   []ReviewedBlock `json:"blocks"`
}

// DescriptionsFile is the descriptions-only judgment step's entire output: the
// one small file the AI writes instead of a full reviewed draft. Notes is one
// plain day-level string; the JSON form accepts a string only (never an
// array), and Descriptions maps each draft block_id to its description prose.
// The PRs list is never part of it: record appends each block's prs_inline.
type DescriptionsFile struct {
	Notes        string            `json:"notes,omitempty"`
	Descriptions map[string]string `json:"descriptions"`
}

// ParseDescriptions parses the descriptions file strictly. A notes field that
// is present must be a JSON string — an array or any other type is a
// validation error, so the AI-facing schema never exposes an array form.
// An absent (or null) notes field is an empty string.
func ParseDescriptions(data []byte) (DescriptionsFile, error) {
	var raw struct {
		Notes        json.RawMessage   `json:"notes"`
		Descriptions map[string]string `json:"descriptions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return DescriptionsFile{}, newError(CodeBadRequest,
			`record: the descriptions file is not valid JSON for the expected schema {"notes": "<string>", "descriptions": {"<block_id>": "<prose>"}}`)
	}
	out := DescriptionsFile{Descriptions: raw.Descriptions}
	if trimmed := strings.TrimSpace(string(raw.Notes)); len(trimmed) > 0 && trimmed != "null" {
		if !strings.HasPrefix(trimmed, `"`) {
			return DescriptionsFile{}, newError(CodeBadRequest,
				`record: the descriptions file's "notes" must be one JSON string, not an array or other type`)
		}
		if err := json.Unmarshal(raw.Notes, &out.Notes); err != nil {
			return DescriptionsFile{}, newError(CodeBadRequest,
				`record: the descriptions file's "notes" must be one JSON string, not an array or other type`)
		}
	}
	return out, nil
}

// ComposeReviewed composes the reviewed draft the record path consumes from
// the draft and the descriptions-only judgment output. Every draft block must
// get a description: a missing or empty one is a validation error listing the
// offending block_ids, and a descriptions entry naming a block_id the draft
// does not contain is a validation error. When a block carries prs_inline and
// its text is not already contained in the description, it is appended to the
// description end with a single space separator (never duplicated; blocks
// without PRs are unchanged). The returned reviewed draft then runs the EXACT
// existing RecordEntries validation and merge path — no validation rule
// changes for either input mode.
func ComposeReviewed(draft Draft, desc DescriptionsFile) (ReviewedDraft, error) {
	known := make(map[string]bool, len(draft.Blocks))
	for _, b := range draft.Blocks {
		known[b.BlockID] = true
	}
	var missing []string
	for _, b := range draft.Blocks {
		if d := desc.Descriptions[b.BlockID]; strings.TrimSpace(d) == "" {
			missing = append(missing, b.BlockID)
		}
	}
	if len(missing) > 0 {
		if desc.Descriptions == nil {
			return ReviewedDraft{}, newError(CodeBadRequest,
				"record: the descriptions file has no descriptions map")
		}
		return ReviewedDraft{}, newError(CodeBadRequest,
			"record: every block needs a description; missing or empty: "+strings.Join(missing, ", "))
	}
	var unknown []string
	for id := range desc.Descriptions {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return ReviewedDraft{}, newError(CodeBadRequest,
			"record: the descriptions file names block ids the draft does not contain: "+strings.Join(unknown, ", "))
	}
	reviewed := ReviewedDraft{
		Date: draft.Date, Timezone: draft.Timezone, DayNote: draft.DayNote,
		Flags: draft.Flags, Notes: desc.Notes,
	}
	for _, b := range draft.Blocks {
		d := desc.Descriptions[b.BlockID]
		// Suffix check, not Contains: a prefix-sharing PR id ("PRs: #12 …"
		// inside "PRs: #123 …") must not make the append look already done.
		// Trailing whitespace of any kind is stripped so the separator is a
		// single space.
		if b.PRsInline != "" && !strings.HasSuffix(strings.TrimRight(d, " \t\n\r"), b.PRsInline) {
			d = strings.TrimRight(d, " \t\n\r") + " " + b.PRsInline
		}
		reviewed.Blocks = append(reviewed.Blocks, ReviewedBlock{DraftBlock: b, Description: d})
	}
	return reviewed, nil
}

// EntryRecord is one stored log entry. It keeps the full source references so
// any entry can be checked against the providers it came from.
type EntryRecord struct {
	BlockID          string            `json:"block_id"`
	Date             string            `json:"date"`
	Start            string            `json:"start,omitempty"`
	End              string            `json:"end,omitempty"`
	Description      string            `json:"description"`
	PRs              []string          `json:"prs,omitempty"`
	Kind             string            `json:"kind"`
	TopicCandidates  []string          `json:"topic_candidates,omitempty"`
	Sources          []DraftSourceItem `json:"sources,omitempty"`
	SourcesTruncated bool              `json:"sources_truncated,omitempty"`
	Completeness     map[string]string `json:"completeness,omitempty"`
	Notes            string            `json:"notes,omitempty"`
	RecordedAt       string            `json:"recorded_at"`
}

// EntriesFile is the durable per-day log. One file per date, merged by block:
// re-recording a block updates it in place and never duplicates it.
type EntriesFile struct {
	Date       string        `json:"date"`
	AppVersion string        `json:"app_version"`
	UpdatedAt  string        `json:"updated_at"`
	Entries    []EntryRecord `json:"entries"`
}

// RecordReceipt reports one recording run. Removed counts the day's prior
// blocks a --replace run dropped (issue #162); merge runs never remove, so
// the field stays absent there.
type RecordReceipt struct {
	Date         string `json:"date"`
	EntriesTotal int    `json:"entries_total"`
	Written      int    `json:"written"`
	Removed      int    `json:"removed,omitempty"`
	Path         string `json:"path"`
}

func EntriesPath(statePath, date string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(statePath)), "entries", date+".json")
}

// RecordEntries validates the reviewed draft and merges it into the day's
// entries file atomically. It is idempotent per block_id: an existing block is
// replaced, other blocks are preserved, and a second run of the same input
// changes nothing but UpdatedAt. With replace (issue #162), the reviewed draft
// is the day's complete content: all blocks for the date are replaced with the
// draft's blocks (block ids from the draft; blocks absent from it are removed
// from the entries file) — the re-collected-day case where the new generation
// supersedes the old. The property within one replace run is unchanged:
// re-running the same replace is idempotent, and per-block validation is
// identical. Before merging it re-attaches each block's
// audit-only source references from the day's durable evidence (the
// AI-visible draft no longer carries them); recording never writes state and
// never blocks on a store-holding process.
func RecordEntries(statePath, appVersion string, reviewed ReviewedDraft, replace bool, now time.Time) (RecordReceipt, error) {
	if _, err := time.Parse("2006-01-02", reviewed.Date); err != nil {
		return RecordReceipt{}, newError(CodeInvalidDate, "record: date must be YYYY-MM-DD")
	}
	if len(reviewed.Blocks) == 0 {
		return RecordReceipt{}, newError(CodeBadRequest, "record: at least one block is required")
	}
	seen := map[string]bool{}
	for _, b := range reviewed.Blocks {
		id := strings.TrimSpace(b.BlockID)
		if id == "" {
			return RecordReceipt{}, newError(CodeBadRequest, "record: every block needs a block_id")
		}
		if seen[id] {
			return RecordReceipt{}, newError(CodeBadRequest, "record: duplicate block_id "+id)
		}
		seen[id] = true
		if strings.TrimSpace(b.Description) == "" {
			return RecordReceipt{}, newError(CodeBadRequest, "record: block "+id+" has an empty description")
		}
		if len(b.PRs) > 0 && !strings.Contains(b.Description, "PRs:") {
			return RecordReceipt{}, newError(CodeBadRequest,
				"record: block "+id+" has PRs but its description does not carry the inline PRs: list")
		}
	}

	reviewed.Blocks = MergeOverlappingBlocks(reviewed.Blocks)
	seen = make(map[string]bool, len(reviewed.Blocks))
	for _, b := range reviewed.Blocks {
		seen[b.BlockID] = true
	}

	if ev, segments, storedZone := readDayForRecord(statePath, reviewed.Date); ev != nil {
		timezone := strings.TrimSpace(reviewed.Timezone)
		if timezone == "" {
			timezone = storedZone
		}
		attachDraftSources(ev, segments, timezone, reviewed.Blocks)
	}

	path := EntriesPath(statePath, reviewed.Date)
	var existing EntriesFile
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &existing); err != nil || existing.Date != reviewed.Date {
			return RecordReceipt{}, newError(CodeStateInvalid, "record: the existing entries file is not a valid file for this date")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return RecordReceipt{}, newError(CodeStateInvalid, "record: the existing entries file cannot be read")
	}

	stamp := now.UTC().Format(time.RFC3339)
	merged := EntriesFile{Date: reviewed.Date, AppVersion: appVersion, UpdatedAt: stamp}
	kept := map[string]EntryRecord{}
	if !replace {
		for _, e := range existing.Entries {
			kept[e.BlockID] = e
		}
	}
	removed := 0
	if replace {
		for _, e := range existing.Entries {
			if !seen[e.BlockID] {
				removed++
			}
		}
	}
	written := 0
	for _, b := range reviewed.Blocks {
		kept[b.BlockID] = EntryRecord{
			BlockID: b.BlockID, Date: reviewed.Date, Start: b.Start, End: b.End,
			Description: strings.TrimSpace(b.Description), PRs: b.PRs, Kind: b.Kind,
			TopicCandidates: b.TopicCandidates, Sources: b.Sources, SourcesTruncated: b.SourcesTruncated,
			Completeness: b.Completeness, Notes: b.Notes, RecordedAt: stamp,
		}
		written++
	}
	for _, e := range kept {
		merged.Entries = append(merged.Entries, e)
	}
	sort.SliceStable(merged.Entries, func(i, j int) bool {
		if merged.Entries[i].Start != merged.Entries[j].Start {
			return merged.Entries[i].Start < merged.Entries[j].Start
		}
		return merged.Entries[i].BlockID < merged.Entries[j].BlockID
	})

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return RecordReceipt{}, newError(CodeStateInvalid, "record: the entries directory cannot be created")
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return RecordReceipt{}, newError(CodeInternal, "record: entries cannot be encoded")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return RecordReceipt{}, newError(CodeStateInvalid, "record: the entries file cannot be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		return RecordReceipt{}, newError(CodeStateInvalid, "record: the entries file cannot be replaced")
	}
	return RecordReceipt{
		Date: reviewed.Date, EntriesTotal: len(merged.Entries), Written: written, Removed: removed, Path: path,
	}, nil
}

// ReadEntries loads the day's entries file. A missing file is (nil, nil).
func ReadEntries(statePath, date string) (*EntriesFile, error) {
	data, err := os.ReadFile(EntriesPath(statePath, date))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, newError(CodeStateInvalid, "the entries file cannot be read")
	}
	var f EntriesFile
	if err := json.Unmarshal(data, &f); err != nil || f.Date != date {
		return nil, newError(CodeStateInvalid, "the entries file is not valid for this date")
	}
	return &f, nil
}

// The stage command converts the day's recorded entries into staged intervals.
// The sidecar maps each block_id to the staged interval_id it became, for
// same-process restage (edit/remove in place instead of duplicating) and for
// audit. It does NOT make staged state survive a process: Day.Staged is
// session-only, so every fresh command re-derives the staged set from the
// entries ledger and re-adds it; the interval ids that re-derivation produces
// are stable in practice only while nothing is saved. Like the entries file
// the sidecar lives under the entries directory, carries no state-schema
// weight, and is never read by the session dispatcher or the plan builder.

// StagePath returns the per-day stage-map sidecar path derived from the state
// path: entries/<date>.stage.json, next to the day's entries file.
func StagePath(statePath, date string) string {
	return strings.TrimSuffix(EntriesPath(statePath, date), ".json") + ".stage.json"
}

// EntrySpanBounds parses one recorded entry's anchors into minutes since
// midnight for the bridge. ok is false when either anchor is not a time shape
// the review carries ("HH:MM" or an RFC3339 stamp); end <= start is NOT an
// error, so the caller must never read it as unparsable. The two cases differ:
// an end BEFORE its start is an overnight span the interval op resolves onto
// the next day, while an end EQUAL to its start is a zero-duration entry the
// caller skips (the interval op refuses a non-positive duration) so one bad
// entry never aborts the day.
func EntrySpanBounds(start, end string) (startMin, endMin int, ok bool) {
	sm, okS := draftClock(start)
	em, okE := draftClock(end)
	if !okS || !okE {
		return 0, 0, false
	}
	return sm, em, true
}

// SplitEntrySpans splits one recorded entry's [start, end] span into contiguous
// chunk spans of at most maxRemoteEntryMinutes minutes each (greedy
// left-to-right; the final chunk carries the remainder, so 479 minutes becomes
// 240 + 235). The Gradion remote's live-verified policy refuses entries longer
// than four hours (issue #167), so the submission bridge stages long recorded
// blocks as chunk intervals — the entries ledger itself keeps the human's true
// block, and each chunk is its own interval so the one-to-one remote binding
// model is preserved. A span that needs no split, or one that is not a parsable
// same-day HH:MM range, returns the single original span; the interval op then
// validates it honestly.
func SplitEntrySpans(start, end string) [][2]string {
	sm, okS := draftClock(start)
	em, okE := draftClock(end)
	if !okS || !okE || em <= sm {
		return [][2]string{{start, end}}
	}
	if em-sm <= maxRemoteEntryMinutes {
		return [][2]string{{start, end}}
	}
	var out [][2]string
	for from := sm; from < em; from += maxRemoteEntryMinutes {
		to := from + maxRemoteEntryMinutes
		if to > em {
			to = em
		}
		out = append(out, [2]string{clockString(from), clockString(to)})
	}
	return out
}

// EntrySpansADayOrMore reports whether a recorded entry's anchors are RFC 3339
// stamps 24 hours or more apart. Such a span crosses more than one midnight (or
// runs past its own clock span), which no interval can express: an interval
// crosses at most one midnight, and the entry's clocks alone would read as a
// shorter span. The caller skips it honestly instead of truncating it. "HH:MM"
// anchors carry no date and never report true.
func EntrySpansADayOrMore(start, end string) bool {
	s, okS := draftStamp(start)
	e, okE := draftStamp(end)
	return okS && okE && e.Sub(s) >= 24*time.Hour
}

// LegacyStageSpans is the #176 chunking, kept so a day saved under it stays
// current after the carry (issue #277, ADR 0011): StageSpans now carries the
// after-midnight remainder past 240 minutes instead of keeping it in the
// crossing chunk. It is consulted only to prove finalized content.
//
// It chunks one stage-time span recorded under date to the Gradion
// four-hour policy (issue #167). A same-day span is exactly SplitEntrySpans.
// An overnight span (end clock before its start clock; issue #176) is chunked
// in real minutes in the day's timezone, because the local interval model
// stages every interval under its starting date: no chunk may start after
// midnight, so the chunk that crosses midnight is the last one. Chunks run
// greedily left to right while the next chunk would still start before
// midnight; the crossing chunk then covers the final 240 minutes, and the
// pre-midnight chunk before it carries the remainder. Every chunk is then at
// most 240 minutes and the chunks cover the recorded span exactly.
//
// When the span runs 240 minutes or more past midnight, no interval that
// starts on date can reach its end within 240 minutes, so the crossing chunk
// keeps the whole remainder (its pre-midnight allocation stays within the
// policy; the plan's remote_entry_too_long guard still judges the next day's
// allocation). A span whose timezone, anchors or chunk boundaries do not
// resolve to unique local times returns the single original span, so the
// interval op validates it honestly.
func LegacyStageSpans(date, timezone, start, end string) [][2]string {
	sm, okS := draftClock(start)
	em, okE := draftClock(end)
	if !okS || !okE || em >= sm {
		return SplitEntrySpans(start, end)
	}
	single := [][2]string{{start, end}}
	loc, err := dayLocation(timezone)
	if err != nil {
		return single
	}
	from, to, err := Interval{Date: date, Start: clockString(sm), End: clockString(em)}.resolveEndpoints(loc)
	if err != nil {
		return single
	}
	midnight, err := localDayStart(nextDate(date), loc)
	if err != nil {
		return single
	}
	limit := time.Duration(maxRemoteEntryMinutes) * time.Minute
	cuts := []time.Time{from}
	for b := from; to.Sub(b) > limit; {
		if next := b.Add(limit); next.Before(midnight) {
			cuts = append(cuts, next)
			b = next
			continue
		}
		if c := to.Add(-limit); c.Before(midnight) {
			cuts = append(cuts, c)
		}
		break
	}
	if len(cuts) == 1 {
		return single
	}
	clocks := make([]string, 0, len(cuts)+1)
	clocks = append(clocks, clockString(sm))
	for _, c := range cuts[1:] {
		clock := c.In(loc).Format("15:04")
		if at, err := resolveLocal(date, clock, loc, "start"); err != nil || !at.Equal(c) {
			return single
		}
		clocks = append(clocks, clock)
	}
	clocks = append(clocks, clockString(em))
	out := make([][2]string, 0, len(clocks)-1)
	for i := 1; i < len(clocks); i++ {
		out = append(out, [2]string{clocks[i-1], clocks[i]})
	}
	return out
}

// EntryCluster is one stage-time union of recorded entries that strictly
// overlap: the contributors in cluster order (head first), the union span as
// same-day clock strings, the kind the union renders, and the joined
// description the staged intervals carry.
//
// BlockID is the id the stage map, the stage receipt and the classification
// content binding name. A single-entry cluster keeps that entry's own id; a
// cluster of two or more takes the union-derived kind-start-end, the same id
// the draft and record assign, so the merged content keeps one identity on
// every path (issue #183). The head is the contributor the receipt's skip
// reasons name.
type EntryCluster struct {
	BlockIDs    []string
	BlockID     string
	Kind        string
	Start, End  string
	Description string
	// NextDay marks a cluster that starts after the date's midnight: only
	// entries stamped on the next date form one (issue #277). Start and End
	// are then next-date clocks, and the whole cluster is carried
	// (SplitCluster); the source date stages none of it.
	NextDay bool
}

// EffectiveEntrySpans resolves one day's recorded entries into stage-time
// clusters. The Gradion remote forbids overlapping entries (live-verified
// constraint), so a ledger day whose blocks overlap (two concurrent POWs, or a
// meeting inside a broad evidence span) may never stage those minutes twice:
// the overlapping blocks become one cluster over the union of their spans,
// whose interval text names every contributor.
//
// Overlap is strict (end == start merely touches), chained overlaps form one
// cluster, and a gap between clusters is never bridged, so the staged set adds
// no uncovered minute. An overnight entry (end clock before its start clock)
// takes part on a timeline that runs past midnight (issue #176), so a block
// inside it joins its cluster instead of colliding at save, while an
// early-morning block of the same date does not overlap it; an overnight
// cluster's End is the next day's clock. Entries whose times do not parse,
// whose end equals their start, or whose stamps are a day or more apart
// (EntrySpansADayOrMore) are absent from the result: the caller owns their
// skip reasons. The entries ledger is never modified; this is the stage-time
// projection only. The four-hour chunking (StageSpans) applies afterwards per
// cluster span. A cluster whose union spans 24 hours or more is absent too
// (see ResolveEntrySpans).
func EffectiveEntrySpans(entries []EntryRecord) []EntryCluster {
	clusters, _ := ResolveEntrySpans(entries)
	return clusters
}

// EffectiveEntrySpansOn is EffectiveEntrySpans for a ledger recorded under
// date: an entry whose RFC 3339 start stamp falls on the next date sits on the
// extended timeline (ResolveEntrySpansOn). Every stage-time derivation uses it.
func EffectiveEntrySpansOn(date string, entries []EntryRecord) []EntryCluster {
	clusters, _, _ := ResolveEntrySpansOn(date, entries)
	return clusters
}

// ResolveEntrySpans is EffectiveEntrySpans plus the contributors it drops
// because their overlapping union spans 24 hours or more (an overnight entry
// chained with same-day blocks, e.g. 06:00→20:00 and 19:00→08:00). An interval
// crosses at most one midnight, so such a union has no honest interval: its
// clock span would wrap and silently lose the day. dayLong lists those block
// ids in cluster order; the caller skips them with their own reason.
func ResolveEntrySpans(entries []EntryRecord) (clusters []EntryCluster, dayLong []string) {
	clusters, dayLong, _ = ResolveEntrySpansOn("", entries)
	return clusters, dayLong
}

// ResolveEntrySpansOn is ResolveEntrySpans for a ledger recorded under date.
// An entry whose RFC 3339 start stamp falls on the next date is placed 24 hours
// later on the date's timeline (issue #277), not 24 hours early at its clock:
// inside an overnight entry it joins that cluster, and on its own it forms a
// cluster that starts after midnight (EntryCluster.NextDay). pastNext lists the
// contributors of clusters that would run past the next date's midnight; no
// carry crosses a second midnight, so the caller skips them with their own
// reason. An empty date places every entry at its clock, as before.
func ResolveEntrySpansOn(date string, entries []EntryRecord) (clusters []EntryCluster, dayLong, pastNext []string) {
	var spans []powSpan
	var skipped []string
	byID := map[string]EntryRecord{}
	next := ""
	if date != "" {
		next = nextDate(date)
	}
	for _, e := range entries {
		sm, okS := draftClock(e.Start)
		em, okE := draftClock(e.End)
		if !okS || !okE || em == sm || EntrySpansADayOrMore(e.Start, e.End) {
			skipped = append(skipped, e.BlockID)
			continue
		}
		if em < sm {
			em += minutesPerDay // overnight: the end is on the next date
		}
		if st, ok := draftStamp(e.Start); ok && next != "" && st.Format("2006-01-02") == next {
			// Stamped after midnight: the next date's clock on this date's
			// extended timeline, never 24 hours early.
			sm += minutesPerDay
			em += minutesPerDay
		}
		byID[e.BlockID] = e
		spans = append(spans, powSpan{id: e.BlockID, kind: e.Kind, startM: sm, endM: em})
	}
	var out []EntryCluster
	for _, cluster := range mergeSpans(spans) {
		head := spans[cluster[0]]
		unionEnd := head.endM
		for _, i := range cluster {
			unionEnd = max(unionEnd, spans[i].endM)
		}
		if unionEnd-head.startM >= minutesPerDay {
			for _, i := range cluster {
				dayLong = append(dayLong, spans[i].id)
				skipped = append(skipped, spans[i].id)
			}
			continue
		}
		if unionEnd > 2*minutesPerDay {
			for _, i := range cluster {
				pastNext = append(pastNext, spans[i].id)
				skipped = append(skipped, spans[i].id)
			}
			continue
		}
		end := head.endM
		kind := head.kind
		c := EntryCluster{
			Start:   clockString(head.startM % minutesPerDay),
			End:     clockString(head.endM % minutesPerDay),
			NextDay: head.startM >= minutesPerDay,
		}
		texts := make([]string, 0, len(cluster))
		for _, i := range cluster {
			s := spans[i]
			c.BlockIDs = append(c.BlockIDs, s.id)
			if s.endM > end {
				end = s.endM
			}
			if s.kind == "meeting" {
				kind = "meeting"
			}
			texts = append(texts, byID[s.id].Description)
		}
		c.Kind = kind
		c.End = clockString(end % minutesPerDay)
		c.Description = joinText(texts...)
		out = append(out, c)
	}
	// Cluster ids are resolved against the ids of the entries the projection
	// excludes, because the caller reports those under their entry ids and the
	// two may never claim one key.
	ids := make([]string, 0, len(out)+len(skipped))
	fixed := make([]bool, 0, len(out)+len(skipped))
	for i := range out {
		if len(out[i].BlockIDs) == 1 {
			ids = append(ids, out[i].BlockIDs[0])
			fixed = append(fixed, true)
			continue
		}
		ids = append(ids, out[i].Kind+"-"+out[i].Start+"-"+out[i].End)
		fixed = append(fixed, false)
	}
	for _, id := range skipped {
		ids = append(ids, id)
		fixed = append(fixed, true)
	}
	ids = resolveBlockIDs(ids, fixed)
	for i := range out {
		out[i].BlockID = ids[i]
	}
	return out, dayLong, pastNext
}

// StageMapFile is the durable block_id → interval-id sidecar for one day.
// Map carries single-interval blocks (the historical shape; old files load
// unchanged); Chunks carries blocks the Gradion four-hour policy split into
// multiple chunk intervals (issue #167). Readers merge both.
type StageMapFile struct {
	Date       string              `json:"date"`
	AppVersion string              `json:"app_version"`
	UpdatedAt  string              `json:"updated_at"`
	Map        map[string]string   `json:"map"`
	Chunks     map[string][]string `json:"chunks,omitempty"`
}

// ReadStageMap loads the day's stage map as block_id → interval ids (one or
// more per block; Chunks wins over a same-key Map entry). A missing file is
// (nil, nil).
func ReadStageMap(statePath, date string) (map[string][]string, error) {
	data, err := os.ReadFile(StagePath(statePath, date))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, newError(CodeStateInvalid, "the stage map file cannot be read")
	}
	var f StageMapFile
	if err := json.Unmarshal(data, &f); err != nil || f.Date != date {
		return nil, newError(CodeStateInvalid, "the stage map file is not valid for this date")
	}
	merged := make(map[string][]string, len(f.Map)+len(f.Chunks))
	for id, iv := range f.Map {
		if iv != "" {
			merged[id] = []string{iv}
		}
	}
	for id, ivs := range f.Chunks {
		if len(ivs) > 0 {
			merged[id] = ivs
		}
	}
	return merged, nil
}

// WriteStageMap persists the day's stage map atomically, mirroring the entries
// file's write discipline: one temporary file, one rename. Single-interval
// blocks keep the historical Map shape; split blocks go to Chunks.
func WriteStageMap(statePath, appVersion, date string, mapping map[string][]string, now time.Time) error {
	if mapping == nil {
		mapping = map[string][]string{}
	}
	f := StageMapFile{
		Date:       date,
		AppVersion: appVersion,
		UpdatedAt:  now.UTC().Format(time.RFC3339),
		Map:        map[string]string{},
	}
	for id, ivs := range mapping {
		if len(ivs) == 1 {
			f.Map[id] = ivs[0]
		} else if len(ivs) > 1 {
			if f.Chunks == nil {
				f.Chunks = map[string][]string{}
			}
			f.Chunks[id] = ivs
		}
	}
	path := StagePath(statePath, date)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return newError(CodeStateInvalid, "the entries directory cannot be created")
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return newError(CodeInternal, "the stage map cannot be encoded")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return newError(CodeStateInvalid, "the stage map file cannot be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		return newError(CodeStateInvalid, "the stage map file cannot be replaced")
	}
	return nil
}

// classify applies reviewed classification selections to the day's staged
// intervals. Like the staged set itself, a selection made on a staged interval
// is session-only: it lives on Day.Staged entries and becomes durable only
// when confirm+save commit the day. Because every command is a one-shot
// process that re-derives the staged set from the entries ledger, the raw
// selections must survive the process boundary on their own: this sidecar —
// entries/<date>.classify.json, next to the entries file — persists the
// interval_id → {catalog_id, classification_id} map, each selection
// content-bound to the recorded block it was chosen for (BlockContentDigest),
// so confirm and save can replay it against a provably identical re-derived
// staged set and fail closed on any entries drift. It is the same pattern as
// the stage map, carries no state-schema weight, and is never read by the
// session dispatcher or the plan builder.

// ChoicesPath returns the per-day classification-choices sidecar path derived
// from the state path: entries/<date>.classify.json, next to the day's entries
// file and stage map.
func ChoicesPath(statePath, date string) string {
	return strings.TrimSuffix(EntriesPath(statePath, date), ".json") + ".classify.json"
}

// ClassificationChoiceEntry is one interval's reviewed selection: the exact
// catalog_id and classification_id the operator chose, both copied verbatim
// from prior receipts, plus the digest of the recorded block content the
// selection was chosen for (BlockContentDigest). The digest is what makes a
// replay safe: staged interval ids re-derive sequentially, so an entries
// shift between classify and confirm/save could silently re-attach a
// selection to the wrong successor interval — the digest fails closed on any
// such drift instead.
type ClassificationChoiceEntry struct {
	CatalogID        string `json:"catalog_id"`
	ClassificationID string `json:"classification_id"`
	ContentDigest    string `json:"content_digest"`
	// Source is where the selection came from (SelectionSource*); it is
	// audit only and never part of the content binding.
	Source string `json:"source,omitempty"`
}

// BlockContentDigest is the deterministic content binding of one staged
// interval's effective block: the SHA-256 hex of the newline-joined date,
// block_id, start, end and description, with one trailing newline. These are
// exactly the fields the staged interval's text is derived from, and for a
// merged cluster they are the union span and joined description, so any change
// to any contributor changes the digest. Any re-derivation whose blocks differ
// from those the selections were chosen against is refused instead of
// replayed.
func BlockContentDigest(date string, e EntryRecord) string {
	return sha256Hex([]byte(strings.Join([]string{
		date, e.BlockID, e.Start, e.End, e.Description,
	}, "\n") + "\n"))
}

// ClassificationChoicesFile is the durable per-day interval_id → selection
// sidecar. Its Selections shape is also the exact schema of classify's --file
// input, so a choices file written from a stage receipt and the persisted
// sidecar are the same document.
type ClassificationChoicesFile struct {
	Date       string                               `json:"date"`
	AppVersion string                               `json:"app_version"`
	UpdatedAt  string                               `json:"updated_at"`
	Selections map[string]ClassificationChoiceEntry `json:"selections"`
}

// ParseClassificationChoices parses classify's --file input strictly: one
// optional selections map keyed by interval_id. An absent or empty selections
// map is valid and means "no selections" — an honest no-op. A selection
// missing either id is a validation error naming the interval, and a file
// that is not valid JSON names the expected schema so it is diagnosable
// without reading it.
func ParseClassificationChoices(data []byte) (map[string]ClassificationChoiceEntry, error) {
	var raw struct {
		Selections map[string]ClassificationChoiceEntry `json:"selections"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, newError(CodeBadRequest,
			`classify: the choices file is not valid JSON for the expected schema {"selections": {"<interval_id>": {"catalog_id": "...", "classification_id": "..."}}}`)
	}
	ids := make([]string, 0, len(raw.Selections))
	for id := range raw.Selections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		sel := raw.Selections[id]
		if strings.TrimSpace(sel.CatalogID) == "" {
			return nil, newError(CodeBadRequest,
				"classify: interval "+id+" is missing catalog_id; every selection must name the catalog it reviewed")
		}
		if strings.TrimSpace(sel.ClassificationID) == "" {
			return nil, newError(CodeBadRequest,
				"classify: interval "+id+" is missing classification_id")
		}
	}
	return raw.Selections, nil
}

// ReadClassificationChoices loads the day's classification choices. A missing
// file is (nil, nil). A present file without a selections key is fail-closed
// state_invalid naming the file and the missing key: silence there would let
// confirm and save proceed unclassified while believing the day was
// classified. An explicit empty map ("selections": {}) remains the valid
// no-op/clear.
func ReadClassificationChoices(statePath, date string) (map[string]ClassificationChoiceEntry, error) {
	data, err := os.ReadFile(ChoicesPath(statePath, date))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, newError(CodeStateInvalid, "the classification choices file cannot be read")
	}
	var f ClassificationChoicesFile
	if err := json.Unmarshal(data, &f); err != nil || f.Date != date {
		return nil, newError(CodeStateInvalid, "the classification choices file is not valid for this date")
	}
	if f.Selections == nil {
		return nil, newError(CodeStateInvalid,
			"the classification choices file ("+ChoicesPath(statePath, date)+") has no selections key; expected {\"selections\": {}} at minimum")
	}
	return f.Selections, nil
}

// WriteClassificationChoices persists the day's classification choices
// atomically, mirroring the entries file's write discipline: one temporary
// file, one rename. The written map replaces any previous one: the latest
// choices file defines the day's selections, so an empty map clears them.
func WriteClassificationChoices(statePath, appVersion, date string, selections map[string]ClassificationChoiceEntry, now time.Time) error {
	if selections == nil {
		selections = map[string]ClassificationChoiceEntry{}
	}
	f := ClassificationChoicesFile{
		Date:       date,
		AppVersion: appVersion,
		UpdatedAt:  now.UTC().Format(time.RFC3339),
		Selections: selections,
	}
	path := ChoicesPath(statePath, date)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return newError(CodeStateInvalid, "the entries directory cannot be created")
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return newError(CodeInternal, "the classification choices cannot be encoded")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return newError(CodeStateInvalid, "the classification choices file cannot be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		return newError(CodeStateInvalid, "the classification choices file cannot be replaced")
	}
	return nil
}

// resolve applies one explicit human identity/content decision against the
// day's current remote observation. The decision replaces the interactive pi
// picker: the human reads the reconcile (or plan) receipt, then authors one
// small JSON file whose fields are copied verbatim from that receipt, and the
// command passes it to the resolve_remote op. Every op-level refusal — a wrong
// or stale observation_id, a remote_id the observation does not carry, adopt
// without confirmed durable local work, a decision that would steal another
// allocation's binding — happens in the op and fails closed; this parser only
// checks the file's own shape.

// ResolutionDecisionFile is resolve's --file input. Revision is the day
// revision copied verbatim from the reconcile receipt (the op fails closed on
// a stale one); ObservationID and RemoteID identify the exact observation and
// observed entry the human reviewed; LocalID names the confirmed durable local
// interval for adopt and accept_observed and must be omitted for foreign.
type ResolutionDecisionFile struct {
	Revision      int    `json:"revision"`
	ObservationID string `json:"observation_id"`
	Decision      string `json:"decision"`
	RemoteID      string `json:"remote_id"`
	LocalID       string `json:"local_id,omitempty"`
}

// ParseResolutionDecision parses resolve's --file input strictly. A file that
// is not valid JSON names the expected schema so it is diagnosable without
// reading it; a missing or negative revision is a validation error (the op
// itself requires the revision and refuses a stale one). The decision value
// and the id relationships are the op's own fail-closed checks, passed through
// verbatim.
func ParseResolutionDecision(data []byte) (ResolutionDecisionFile, error) {
	var raw struct {
		Revision      *int   `json:"revision"`
		ObservationID string `json:"observation_id"`
		Decision      string `json:"decision"`
		RemoteID      string `json:"remote_id"`
		LocalID       string `json:"local_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ResolutionDecisionFile{}, newError(CodeBadRequest,
			`resolve: the decision file is not valid JSON for the expected schema {"revision": <n>, "observation_id": "...", "decision": "adopt|accept_observed|foreign", "remote_id": "...", "local_id": "..."} (local_id omitted for foreign)`)
	}
	if raw.Revision == nil {
		return ResolutionDecisionFile{}, newError(CodeBadRequest,
			"resolve: the decision file is missing revision; copy it verbatim from the reconcile receipt")
	}
	if *raw.Revision < 0 {
		return ResolutionDecisionFile{}, newError(CodeBadRequest,
			"resolve: the decision file's revision must not be negative")
	}
	return ResolutionDecisionFile{
		Revision: *raw.Revision, ObservationID: raw.ObservationID, Decision: raw.Decision,
		RemoteID: raw.RemoteID, LocalID: raw.LocalID,
	}, nil
}

// MergeOverlappingBlocks folds every connected component of strictly
// overlapping reviewed blocks into one block over the union span, so a
// reviewed file records the blocks the draft shows (issue #183 rule 5).
// Touching blocks stay separate, chained overlaps join one cluster, and a day
// without overlap comes back unchanged, which makes a second call a no-op.
//
// A multi-member cluster takes the union-derived id (kind, union start, union
// end), the same one the draft assigns, so recording the draft after a
// recovery merge replaces that entry instead of writing a second one under the
// head's old id. Singleton blocks keep their own id. A cluster renders a
// meeting when it holds one and the head's kind otherwise, and folds every
// contributor's title and description through joinText, its topics and PRs
// (deduped and sorted) and its sources under the same per-block cap the topic
// fold uses. Blocks whose times are empty, unparsable or not increasing are
// not candidates and keep their own fields.
func MergeOverlappingBlocks(blocks []ReviewedBlock) []ReviewedBlock {
	startM := make([]int, len(blocks))
	endM := make([]int, len(blocks))
	var spans []powSpan
	var at []int
	for i := range blocks {
		sm, okS := draftClock(blocks[i].Start)
		em, okE := draftClock(blocks[i].End)
		if !okS || !okE || em <= sm {
			continue
		}
		startM[i], endM[i] = sm, em
		spans = append(spans, powSpan{
			id: blocks[i].BlockID, kind: blocks[i].Kind, startM: sm, endM: em,
		})
		at = append(at, i)
	}
	if len(spans) == 0 {
		return blocks
	}
	out := make([]ReviewedBlock, 0, len(blocks))
	var multi []bool
	inCluster := make([]bool, len(blocks))
	for _, cluster := range mergeSpans(spans) {
		members := make([]int, len(cluster))
		for k, si := range cluster {
			members[k] = at[si]
			inCluster[at[si]] = true
		}
		out = append(out, foldReviewedCluster(blocks, members, startM, endM))
		multi = append(multi, len(members) > 1)
	}
	for i := range blocks {
		if !inCluster[i] {
			out = append(out, blocks[i])
			multi = append(multi, false)
		}
	}
	ids := make([]string, len(out))
	fixed := make([]bool, len(out))
	for i := range out {
		if !multi[i] {
			ids[i], fixed[i] = out[i].BlockID, true
			continue
		}
		ids[i] = out[i].Kind + "-" + out[i].Start + "-" + out[i].End
	}
	ids = resolveBlockIDs(ids, fixed)
	for i := range out {
		out[i].BlockID = ids[i]
	}
	return out
}

// foldReviewedCluster renders one reviewed cluster. A single-member cluster is
// returned untouched, so a reviewed file with no overlap records byte for byte
// what it carried.
func foldReviewedCluster(blocks []ReviewedBlock, members []int, startM, endM []int) ReviewedBlock {
	head := blocks[members[0]]
	if len(members) == 1 {
		return head
	}
	out := ReviewedBlock{
		DraftBlock: DraftBlock{
			BlockID:      head.BlockID,
			Kind:         head.Kind,
			Start:        clockString(startM[members[0]]),
			Completeness: head.Completeness,
		},
		Notes: head.Notes,
	}
	end := endM[members[0]]
	hasMeeting := false
	for _, i := range members {
		if blocks[i].Kind == "meeting" {
			hasMeeting = true
			break
		}
	}
	var titles, absorbedTitles, meetingTitles, descs []string
	for _, i := range members {
		b := blocks[i]
		if endM[i] > end {
			end = endM[i]
		}
		if b.Kind == "meeting" {
			out.Kind = "meeting"
			meetingTitles = append(meetingTitles, b.Title)
		} else {
			if hasMeeting && len(b.TopicCandidates) == 0 {
				absorbedTitles = append(absorbedTitles, b.Title)
			}
			title := b.Title
			if strings.TrimSpace(title) == "" && len(b.TopicCandidates) > 0 {
				title = b.TopicCandidates[0]
			}
			titles = append(titles, title)
		}
		descs = append(descs, b.Description)
		out.TopicCandidates = append(out.TopicCandidates, b.TopicCandidates...)
		for _, pr := range b.PRs {
			if !containsString(out.PRs, pr) {
				out.PRs = append(out.PRs, pr)
			}
		}
		for _, src := range b.Sources {
			if len(out.Sources) < draftSourcesPerBlock {
				out.Sources = append(out.Sources, src)
			} else {
				out.SourcesTruncated = true
			}
		}
		out.SourcesTruncated = out.SourcesTruncated || b.SourcesTruncated
	}
	out.End = clockString(end)
	if out.Kind == "meeting" {
		// Every meeting title survives: a recovery file that happens to hold two
		// overlapping meetings must not lose the second one's name.
		out.Title = joinText(append(meetingTitles, absorbedTitles...)...)
	} else {
		out.Title = joinText(titles...)
	}
	out.Description = joinText(descs...)
	sortPRs(out.PRs)
	out.PRsInline = prsInline(out.PRs)
	return out
}

// stateEvidenceView is a minimal read-only projection of the durable state
// file: record parses the evidence section and every day's timezone and
// confirmed intervals, so it can render the requested day exactly as review
// does, and never anything else.
type stateEvidenceView struct {
	Evidence map[string]*DayEvidence `json:"evidence"`
	Days     map[string]struct {
		Timezone  string     `json:"timezone"`
		Confirmed []Interval `json:"confirmed"`
	} `json:"days"`
}

// readDayForRecord reads one day's durable evidence, its rendered confirmed
// segments and its timezone for record-time source re-attachment. The segments
// come from the same renderSegments review uses over every stored day's
// confirmed intervals, so a previous day's cross-midnight spill appears here
// exactly as it appears in the reviewed draft. It never takes the store's
// single-writer lock and never writes state, so recording still never blocks
// on, or interferes with, a store-holding process (the state file is replaced
// atomically, so a lock-free read sees one complete version). A missing,
// unreadable, or invalid state file yields nil evidence: re-attachment is audit
// parity, never a record precondition, and the day still records.
func readDayForRecord(statePath, date string) (*DayEvidence, []Segment, string) {
	if statePath == "" || date == "" {
		return nil, nil, ""
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, nil, ""
	}
	var view stateEvidenceView
	if err := json.Unmarshal(data, &view); err != nil {
		return nil, nil, ""
	}
	origins := make(map[string]originDay, len(view.Days))
	for originDate, day := range view.Days {
		origins[originDate] = originDay{Date: originDate, Timezone: day.Timezone, Intervals: day.Confirmed}
	}
	segments, err := renderSegments(date, origins)
	if err != nil {
		return nil, nil, ""
	}
	// The day's stored timezone is the authority for anchor conversion, so a
	// recovery file that dropped the key cannot silently re-attach sources on
	// a different clock than the draft used.
	return view.Evidence[date], segments, view.Days[date].Timezone
}

// recordDiagnostics is where record writes non-fatal source-attachment notes.
// It defaults to stderr and is a variable so tests can capture the exact lines;
// the command's stdout and receipt never carry them.
var recordDiagnostics io.Writer = os.Stderr

// attachDraftSources gives each recorded block the audit-only sources the draft
// attached to the block with the same id. Ownership is recomputed by
// partitionDay from the same durable inputs BuildDraft saw (the day's evidence
// and the confirmed segments review rendered), never reconstructed from the
// reviewed file's post-merge shape: the merge is lossy for ownership, so a
// cluster that absorbed an evidence block would otherwise reorder its sources
// or rebuild a shrunken evidence span (issue #183 rule 5).
//
// A block id the recompute does not produce keeps whatever the reviewed file
// carried. The AI-visible reviewed file carries no sources, so a stale or
// hand-edited block records empty sources rather than a guessed assignment, and
// record says so on stderr instead of leaving the mismatch silent.
func attachDraftSources(ev *DayEvidence, segments []Segment, timezone string, blocks []ReviewedBlock) {
	partition := partitionDay(ev.Topics, ev.Meetings, segments, timezone)
	byID := make(map[string]DraftBlock, len(partition.Blocks))
	for _, b := range partition.Blocks {
		byID[b.blk.BlockID] = b.blk
	}
	for i := range blocks {
		recomputed, ok := byID[blocks[i].BlockID]
		if !ok {
			if len(blocks[i].Sources) == 0 {
				fmt.Fprintf(recordDiagnostics,
					"record: block %s is not in the recomputed draft; the state no longer matches the draft, so no sources were attached\n",
					blocks[i].BlockID)
			}
			continue
		}
		blocks[i].Sources = recomputed.Sources
		blocks[i].SourcesTruncated = recomputed.SourcesTruncated
	}
}

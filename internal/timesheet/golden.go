package timesheet

import (
	"sort"
	"strings"
)

// GoldenSchema 2 (#279) records the catalog candidates each classifications
// input was judged against, so eval run can replay it offline.
const GoldenSchema = 2

// GoldenDataset is the exported file. It holds the owner's evidence and stays
// on the owner's machine: never commit it.
type GoldenDataset struct {
	Schema     int          `json:"schema"`
	ExportedAt string       `json:"exported_at"`
	Days       []GoldenDay  `json:"days"`
	Skipped    []GoldenSkip `json:"skipped"`
	Note       string       `json:"note,omitempty"`
}

// GoldenDay is one past day. Either judgment may be absent when its input or
// truth was unavailable; the reason is then listed in GoldenDataset.Skipped.
type GoldenDay struct {
	Date            string                 `json:"date"`
	Descriptions    *GoldenDescriptions    `json:"descriptions,omitempty"`
	Classifications *GoldenClassifications `json:"classifications,omitempty"`
}

// GoldenDescriptions pairs the draft the descriptions judgment saw with the
// accepted description per block_id.
type GoldenDescriptions struct {
	Input Draft             `json:"input"`
	Truth map[string]string `json:"truth"`
	// RemoteTruth lists the block_ids whose truth is the submitted remote
	// text rather than the local ledger (a later remote edit wins).
	RemoteTruth []string `json:"remote_truth,omitempty"`
}

// GoldenBlock is one staged interval as the classifications judgment sees it.
type GoldenBlock struct {
	IntervalID  string `json:"interval_id"`
	BlockID     string `json:"block_id"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Description string `json:"description"`
	// Candidates is the ref of this interval's set in
	// GoldenClassifications.Candidates; empty when it had none.
	Candidates string `json:"candidates,omitempty"`
}

// GoldenClassifications pairs the intervals the classifications judgment saw
// (the staged blocks minus those decided from approved history) and the
// catalog candidate sets it picked from with the accepted classification_id
// per interval_id.
type GoldenClassifications struct {
	Input       []GoldenBlock        `json:"input"`
	Candidates  []GoldenCandidateSet `json:"candidates"`
	Truth       map[string]string    `json:"truth"`
	RemoteTruth []string             `json:"remote_truth,omitempty"`
}

// GoldenCandidateSet is one prefetched catalog option list, as recorded in
// the day's candidates.json when the day was judged.
type GoldenCandidateSet struct {
	Ref       string         `json:"ref"`
	CatalogID string         `json:"catalog_id"`
	Options   []GoldenOption `json:"options"`
}

type GoldenOption struct {
	ClassificationID string `json:"classification_id"`
	Label            string `json:"label"`
	Code             string `json:"code,omitempty"`
}

// GoldenSkip is one day part left out of the dataset. Skipped parts never
// count as passes: the scorer only sees what the dataset includes.
type GoldenSkip struct {
	Date   string `json:"date"`
	Part   string `json:"part"`
	Reason string `json:"reason"`
}

// GoldenCandidate is a set of answers to score, keyed by date: descriptions
// by block_id and classification_ids by interval_id.
type GoldenCandidate struct {
	Days map[string]GoldenAnswer `json:"days"`
}

type GoldenAnswer struct {
	Descriptions    map[string]string `json:"descriptions,omitempty"`
	Classifications map[string]string `json:"classifications,omitempty"`
}

// AcceptedCandidate is the dataset's own truth as a candidate: scoring it is
// the 100% self-check.
func AcceptedCandidate(g GoldenDataset) GoldenCandidate {
	c := GoldenCandidate{Days: map[string]GoldenAnswer{}}
	for _, d := range g.Days {
		var a GoldenAnswer
		if d.Descriptions != nil {
			a.Descriptions = d.Descriptions.Truth
		}
		if d.Classifications != nil {
			a.Classifications = d.Classifications.Truth
		}
		c.Days[d.Date] = a
	}
	return c
}

// GoldenScore is the scorer's report. Rates are in [0,1]; a rate over zero
// items is reported as 0 with its zero count, never as a pass.
type GoldenScore struct {
	Days int `json:"days"`

	ClassificationIntervals int     `json:"classification_intervals"`
	ClassificationMatches   int     `json:"classification_matches"`
	ClassificationExactRate float64 `json:"classification_exact_rate"`
	// ClassificationUnwinnable counts intervals whose accepted answer the
	// judge could not pick: no candidate set, or not among its set's options.
	// They stay in the rate as misses; the count says how many were certain.
	ClassificationUnwinnable int `json:"classification_unwinnable"`

	DescriptionBlocks     int     `json:"description_blocks"`
	DescriptionAnswered   int     `json:"description_answered"`
	DescriptionCoverage   float64 `json:"description_coverage"`
	DescriptionSimilarity float64 `json:"description_similarity"`

	// Misses names each wrong or missing item, date-ordered, bounded.
	Misses []string `json:"misses,omitempty"`
}

const maxScoreMisses = 50

// ScoreGolden rates candidate against the dataset's accepted answers:
// classifications by exact match per interval, descriptions by coverage
// (every block answered with non-empty text) and mean word-set Jaccard
// similarity against the accepted text. A missing answer scores zero.
func ScoreGolden(g GoldenDataset, candidate GoldenCandidate) GoldenScore {
	var s GoldenScore
	var simSum float64
	miss := func(m string) {
		if len(s.Misses) < maxScoreMisses {
			s.Misses = append(s.Misses, m)
		}
	}
	for _, d := range g.Days {
		s.Days++
		ans := candidate.Days[d.Date]
		if c := d.Classifications; c != nil {
			winnable := winnablePicks(c)
			for _, id := range goldenKeys(c.Truth) {
				s.ClassificationIntervals++
				if !winnable[id+"\x00"+c.Truth[id]] {
					s.ClassificationUnwinnable++
				}
				if got := ans.Classifications[id]; got == c.Truth[id] {
					s.ClassificationMatches++
				} else {
					miss(d.Date + " classification " + id + ": got " + quoteOrNone(got) + ", accepted " + c.Truth[id])
				}
			}
		}
		if ds := d.Descriptions; ds != nil {
			for _, id := range goldenKeys(ds.Truth) {
				s.DescriptionBlocks++
				got := strings.TrimSpace(ans.Descriptions[id])
				if got == "" {
					miss(d.Date + " description " + id + ": unanswered")
					continue
				}
				s.DescriptionAnswered++
				simSum += WordJaccard(got, ds.Truth[id])
			}
		}
	}
	s.ClassificationExactRate = rate(s.ClassificationMatches, s.ClassificationIntervals)
	s.DescriptionCoverage = rate(s.DescriptionAnswered, s.DescriptionBlocks)
	if s.DescriptionBlocks > 0 {
		s.DescriptionSimilarity = simSum / float64(s.DescriptionBlocks)
	}
	return s
}

// winnablePicks is every interval_id + "\x00" + classification_id the judge
// could answer: an option of the set the interval names.
func winnablePicks(c *GoldenClassifications) map[string]bool {
	options := map[string][]GoldenOption{}
	for _, s := range c.Candidates {
		options[s.Ref] = s.Options
	}
	out := map[string]bool{}
	for _, b := range c.Input {
		if b.Candidates == "" {
			continue
		}
		for _, o := range options[b.Candidates] {
			out[b.IntervalID+"\x00"+o.ClassificationID] = true
		}
	}
	return out
}

// WordJaccard is |A∩B|/|A∪B| over lower-cased word sets; two empty texts are
// identical (1).
func WordJaccard(a, b string) float64 {
	wa, wb := wordSet(a), wordSet(b)
	if len(wa) == 0 && len(wb) == 0 {
		return 1
	}
	inter := 0
	for w := range wa {
		if wb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(wa)+len(wb)-inter)
}

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	}) {
		out[w] = true
	}
	return out
}

func rate(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func goldenKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func quoteOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return `"` + s + `"`
}

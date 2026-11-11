package timesheet

import (
	"math"
	"testing"
)

func syntheticGolden() GoldenDataset {
	return GoldenDataset{Schema: GoldenSchema, Days: []GoldenDay{{
		Date: "2026-10-19",
		Descriptions: &GoldenDescriptions{Truth: map[string]string{
			"b1": "Build the synthetic bridge", "b2": "Review the synthetic deck",
		}},
		Classifications: &GoldenClassifications{Truth: map[string]string{"iv-1": "cls-a", "iv-2": "cls-b"}},
	}}}
}

func TestScoreGoldenSelfCheckIsPerfect(t *testing.T) {
	g := syntheticGolden()
	s := ScoreGolden(g, AcceptedCandidate(g))
	if s.ClassificationExactRate != 1 || s.DescriptionCoverage != 1 || s.DescriptionSimilarity != 1 || len(s.Misses) != 0 {
		t.Fatalf("self score = %+v, want 100%%", s)
	}
}

func TestScoreGoldenCountsMissesAndSimilarity(t *testing.T) {
	g := syntheticGolden()
	c := GoldenCandidate{Days: map[string]GoldenAnswer{"2026-10-19": {
		Descriptions:    map[string]string{"b1": "Build the bridge"},
		Classifications: map[string]string{"iv-1": "cls-a", "iv-2": "cls-x"},
	}}}
	s := ScoreGolden(g, c)
	if s.ClassificationIntervals != 2 || s.ClassificationMatches != 1 || s.ClassificationExactRate != 0.5 {
		t.Fatalf("classifications = %+v", s)
	}
	if s.DescriptionBlocks != 2 || s.DescriptionAnswered != 1 || s.DescriptionCoverage != 0.5 {
		t.Fatalf("descriptions = %+v", s)
	}
	// b1: {build,the,bridge} vs {build,the,synthetic,bridge} = 3/4; b2 unanswered = 0.
	if want := 0.375; math.Abs(s.DescriptionSimilarity-want) > 1e-9 {
		t.Fatalf("similarity = %v, want %v", s.DescriptionSimilarity, want)
	}
	if len(s.Misses) != 2 {
		t.Fatalf("misses = %v, want the wrong classification and the unanswered block", s.Misses)
	}
}

func TestScoreGoldenCountsUnwinnableIntervals(t *testing.T) {
	g := GoldenDataset{Schema: GoldenSchema, Days: []GoldenDay{{
		Date: "2026-10-19",
		Classifications: &GoldenClassifications{
			Input:      []GoldenBlock{{IntervalID: "iv-1", Candidates: "c1"}, {IntervalID: "iv-2", Candidates: "c1"}, {IntervalID: "iv-3"}},
			Candidates: []GoldenCandidateSet{{Ref: "c1", CatalogID: "cat", Options: []GoldenOption{{ClassificationID: "cls-a"}}}},
			Truth:      map[string]string{"iv-1": "cls-a", "iv-2": "cls-b", "iv-3": "cls-c"},
		},
	}}}
	if s := ScoreGolden(g, AcceptedCandidate(g)); s.ClassificationUnwinnable != 2 {
		t.Fatalf("unwinnable = %d, want the off-list truth and the interval with no set", s.ClassificationUnwinnable)
	}
}

func TestScoreGoldenEmptyDatasetIsNeverAPass(t *testing.T) {
	s := ScoreGolden(GoldenDataset{Schema: GoldenSchema}, GoldenCandidate{})
	if s.Days != 0 || s.ClassificationExactRate != 0 || s.DescriptionCoverage != 0 {
		t.Fatalf("empty score = %+v, want zero counts and zero rates", s)
	}
}

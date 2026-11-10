package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// bindCandidates writes a candidates.json for the day's current staged ledger
// offline, with every interval pointing at one set over a seeded catalog. It
// stands in for the live candidates step in tests that have no catalog server.
func bindCandidates(t *testing.T, cfg, day, catalogID string, options ...timesheet.ClassificationOption) {
	t.Helper()
	blocks, digest, err := stagedDayBlocks(statePathFor(cfg), day)
	if err != nil {
		t.Fatal(err)
	}
	set := nextCandidateSet{Ref: "c1", Query: "seeded", CatalogID: catalogID, Complete: true}
	for _, o := range options {
		set.Options = append(set.Options, nextCandidateOption{ClassificationID: o.ID, Label: o.Name, Code: o.Code})
	}
	f := candidatesFile{LedgerDigest: digest, Sets: []nextCandidateSet{set}, Intervals: map[string]string{}}
	for id := range blocks {
		f.Intervals[id] = "c1"
	}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	writeAnswerFile(t, filepath.Join(testArtifactDir(t, cfg, day), "candidates.json"), string(data))
}

// nextJudgmentDoc derives once and, when the planner asks for the candidates
// step, binds seeded candidates offline and derives again.
func nextJudgmentDoc(t *testing.T, cfg, catalogID string, args ...string) nextAction {
	t.Helper()
	n := nextDoc(t, args...)
	if n.State == stateAction && n.Step == "candidates" {
		bindCandidates(t, cfg, testDate, catalogID)
		n = nextDoc(t, args...)
	}
	return n
}

func TestCandidateQueryStripsPRsAndBoundsLength(t *testing.T) {
	cases := map[string]string{
		"Build the bridge. PRs: org/repo#12": "Build the bridge",
		"  Review   the deck\nsecond line":   "Review the deck",
		"":                                   "",
		strings.Repeat("x", 200):             strings.Repeat("x", maxQueryRunes),
		"Ship it; PRs: a#1; b#2":             "Ship it",
	}
	for in, want := range cases {
		if got := candidateQuery(timesheet.EntryRecord{Description: in}); got != want {
			t.Errorf("candidateQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveRoutesCandidatesBeforeTheClassificationJudgment(t *testing.T) {
	for name, mutate := range map[string]func(*nextObservation){
		"absent": func(o *nextObservation) { o.Candidates = nil },
		"stale":  func(o *nextObservation) { o.Candidates = &candidatesFile{LedgerDigest: "old"} },
	} {
		t.Run(name, func(t *testing.T) {
			o := unclassifiedDay()
			mutate(&o)
			n := deriveDayNext(o)
			if n.State != stateAction || n.Step != "candidates" {
				t.Fatalf("state/step = %q/%q, want action/candidates", n.State, n.Step)
			}
			assertCmd(t, n, withArgs(baseArgv("classifications"), "--candidates"))
		})
	}
}

func TestClassificationJudgmentEmbedsBoundCandidates(t *testing.T) {
	o := unclassifiedDay()
	o.Candidates.Sets = append(o.Candidates.Sets, nextCandidateSet{Ref: "c2", CatalogID: "unused"})
	o.Candidates.Note = "1 interval(s) beyond the first 12 distinct topics carry no candidates"
	n := deriveDayNext(o)
	j := n.Judgment
	if n.State != stateJudgment || j == nil || j.Kind != judgmentClassifications {
		t.Fatalf("derivation = %+v, want the classifications judgment", n)
	}
	if len(j.Candidates) != 1 || j.Candidates[0].Ref != "c1" || len(j.Candidates[0].Options) == 0 {
		t.Fatalf("candidates = %+v, want only the referenced set", j.Candidates)
	}
	if j.Blocks[0].Candidates != "c1" {
		t.Fatalf("block = %+v, want its candidate ref", j.Blocks[0])
	}
	if j.CandidatesNote == "" || strings.Contains(j.Protocol, "--query") || !strings.Contains(j.Protocol, "do not query the catalog") {
		t.Fatalf("judgment note/protocol = %q / %q", j.CandidatesNote, j.Protocol)
	}
}

const stubCatalogText = "2 classifications found\n" +
	"- Development (DEV) [engineering] · cls-dev\n" +
	"- Operations (OPS) [ops] · cls-ops\n"

func candidatesFixture(t *testing.T, stub *stubRemote, blocks ...timesheet.ReviewedBlock) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeTestConfigWithGradion(t, server.URL)
	seedEntries(t, cfg, testDate, blocks)
	runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	return cfg
}

func classificationCalls(stub *stubRemote) int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	n := 0
	for _, c := range stub.calls {
		if c == "timesheet__get_classifications" {
			n++
		}
	}
	return n
}

func TestCandidatesStepReadsOncePerTopicAndClassifyAcceptsAPick(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}, classifications: stubCatalogText}
	cfg := candidatesFixture(t, stub,
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
		timedBlock("work-2", "11:00", "12:00", "Build the bridge"),
		timedBlock("work-3", "13:00", "14:00", "Review the deck"),
		timedBlock("work-4", "15:00", "16:00", "PRs: org/repo#1"),
	)
	args := []string{"--config", cfg, "--date", testDate}
	step := nextDoc(t, args...)
	if step.Step != "candidates" {
		t.Fatalf("step = %+v, want candidates", step)
	}
	out, _, err := captureOutput(t, func() error { return runClassifications(step.Action.Cmd[2:]) })
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	var receipt candidatesReceipt
	if err := json.Unmarshal(out, &receipt); err != nil || !filepath.IsAbs(receipt.File) {
		t.Fatalf("receipt = %s, %v", out, err)
	}
	if got := classificationCalls(stub); got != 2 {
		t.Fatalf("catalog reads = %d, want one per distinct topic (2)", got)
	}

	j := nextDoc(t, args...).Judgment
	if j == nil || len(j.Candidates) != 2 {
		t.Fatalf("judgment = %+v, want two candidate sets", j)
	}
	refs := map[string]string{}
	for _, b := range j.Blocks {
		refs[b.BlockID] = b.Candidates
	}
	if refs["work-1"] == "" || refs["work-1"] != refs["work-2"] || refs["work-3"] == refs["work-1"] {
		t.Fatalf("block refs = %v, want shared refs for the same topic", refs)
	}
	if b := findBlock(j.Blocks, "work-4"); b == nil || b.Candidates != "" {
		t.Fatalf("empty-description block = %+v, want no candidates for a PRs-only description", b)
	}
	set := j.Candidates[0]
	if set.CatalogID == "" || !set.Complete || len(set.Options) != 2 || set.Options[0].Label != "Development" {
		t.Fatalf("set = %+v, want the live catalog's options", set)
	}

	pick := findBlock(j.Blocks, "work-1")
	writeAnswerFile(t, j.AnswerFile, `{"ledger_digest":"`+j.LedgerDigest+`","selections":{"`+pick.IntervalID+
		`":{"catalog_id":"`+set.CatalogID+`","classification_id":"`+set.Options[0].ClassificationID+`"}}}`)
	step = nextDoc(t, args...)
	if step.Step != "classify" {
		t.Fatalf("step = %+v, want classify for the bound pick", step)
	}
	executable(t, step)
}

func TestCandidatesStepFailsClosedOnCatalogError(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}, classificationsError: true}
	cfg := candidatesFixture(t, stub, timedBlock("work-1", "09:00", "10:00", "Build the bridge"))
	err := runClassifications([]string{"--config", cfg, "--date", testDate, "--candidates"})
	if err == nil {
		t.Fatal("candidates succeeded over a failed catalog read")
	}
	if _, statErr := os.Stat(filepath.Join(testArtifactDir(t, cfg, testDate), "candidates.json")); !os.IsNotExist(statErr) {
		t.Fatalf("a failed read left candidates.json behind: %v", statErr)
	}
	if got := nextDoc(t, "--config", cfg, "--date", testDate); got.Step != "candidates" {
		t.Fatalf("after failure = %+v, want candidates again", got)
	}
}

func TestCandidatesCapsOptionsAndMarksIncomplete(t *testing.T) {
	var text strings.Builder
	text.WriteString("15 classifications found\n")
	for i := 0; i < 15; i++ {
		text.WriteString("- Option" + string(rune('A'+i)) + " (C" + string(rune('A'+i)) + ") [x] · cls-" + string(rune('a'+i)) + "\n")
	}
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}, classifications: text.String()}
	cfg := candidatesFixture(t, stub, timedBlock("work-1", "09:00", "10:00", "Build the bridge"))
	if _, _, err := captureOutput(t, func() error {
		return runClassifications([]string{"--config", cfg, "--date", testDate, "--candidates"})
	}); err != nil {
		t.Fatal(err)
	}
	j := nextDoc(t, "--config", cfg, "--date", testDate).Judgment
	if j == nil || len(j.Candidates) != 1 {
		t.Fatalf("judgment = %+v", j)
	}
	if s := j.Candidates[0]; len(s.Options) != maxCandidateOptions || s.Complete || s.Note == "" {
		t.Fatalf("set = %+v, want %d options marked incomplete", s, maxCandidateOptions)
	}
}

func TestClassificationsRejectsQueryWithCandidates(t *testing.T) {
	cfg := writeTestConfigWithGradion(t, "http://127.0.0.1:1/mcp")
	err := runClassifications([]string{"--config", cfg, "--query", "x", "--candidates"})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v, want the exclusive-flags refusal", err)
	}
}

func TestNextDoRunsCandidatesInProcessAndStopsAtTheJudgment(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}, classifications: stubCatalogText}
	cfg := candidatesFixture(t, stub, timedBlock("work-1", "09:00", "10:00", "Build the bridge"))
	rep, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--do")
	if err != nil {
		t.Fatalf("next --do: %v", err)
	}
	if len(rep.Ran) != 1 || rep.Ran[0].Step != "candidates" || rep.Ran[0].Result != "ok" {
		t.Fatalf("ran = %+v, want the candidates act", rep.Ran)
	}
	if rep.State != stateJudgment || rep.Judgment == nil || len(rep.Judgment.Candidates) != 1 {
		t.Fatalf("report = %+v, want the classifications judgment with candidates", rep.nextAction)
	}
}

func findBlock(blocks []nextBlock, id string) *nextBlock {
	for i := range blocks {
		if blocks[i].BlockID == id {
			return &blocks[i]
		}
	}
	return nil
}

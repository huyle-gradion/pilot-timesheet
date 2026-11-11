package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func nextDoc(t *testing.T, args ...string) nextAction {
	t.Helper()
	out, _, err := captureOutput(t, func() error { return runNext(args) })
	if err != nil {
		t.Fatalf("next %v: %v", args, err)
	}
	var n nextAction
	if err := json.Unmarshal(out, &n); err != nil {
		t.Fatalf("next did not print one typed JSON action: %v\n%s", err, out)
	}
	if n.Day == "" {
		t.Fatalf("next printed no day: %s", out)
	}
	return n
}

func executable(t *testing.T, n nextAction) {
	t.Helper()
	if n.State != stateAction || n.Action == nil {
		t.Fatalf("state = %q, want one executable action", n.State)
	}
	cmd := n.Action.Cmd
	if len(cmd) < 2 || !filepath.IsAbs(cmd[0]) {
		t.Fatalf("derived argv %#v does not start with an absolute executable", cmd)
	}
	flows := map[string]func([]string) error{
		"run": runRun, "record": runRecord, "stage": runStage, "classify": runClassify, "save": runSave,
	}
	flow, ok := flows[cmd[1]]
	if !ok {
		t.Fatalf("test refuses to execute the non-staging verb %q", cmd[1])
	}
	if _, stderr, err := captureOutput(t, func() error { return flow(cmd[2:]) }); err != nil {
		t.Fatalf("execute %q: %v\n%s", strings.Join(cmd, " "), err, stderr)
	}
}

func writeAnswerFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the judgment answer file: %v", err)
	}
}

func writeNextDescriptions(t *testing.T, draftPath, answerFile, draftDigest string) {
	t.Helper()
	data, err := os.ReadFile(draftPath)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	var draft timesheet.Draft
	if err := json.Unmarshal(data, &draft); err != nil {
		t.Fatalf("draft JSON: %v", err)
	}
	if len(draft.Blocks) == 0 {
		t.Fatalf("draft carries no blocks: %s", data)
	}
	descriptions := map[string]string{}
	for _, b := range draft.Blocks {
		descriptions[b.BlockID] = "Synthetic judgment for " + b.BlockID
	}
	body, err := json.Marshal(map[string]any{
		"draft_digest": draftDigest, "notes": "synthetic day", "descriptions": descriptions,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeAnswerFile(t, answerFile, string(body))
}

func TestNextProgressesToSaveAndHumanGates(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	srv, _ := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)
	day := "2026-10-20"
	args := []string{"--config", cfg, "--env-file", envFile, "--date", day}

	step := nextDoc(t, args...)
	if step.State != stateAction || step.Step != "run" {
		t.Fatalf("initial state/step = %q/%q, want action/run", step.State, step.Step)
	}
	if step.Action.Cmd[1] != "run" || !strings.Contains(strings.Join(step.Action.Cmd, " "), "--date "+day) {
		t.Fatalf("derived argv = %#v, want run with the day pinned", step.Action.Cmd)
	}
	executable(t, step)

	step = nextDoc(t, args...)
	if step.State != stateJudgment || step.Judgment == nil || step.Judgment.Kind != judgmentBlockDescriptions {
		t.Fatalf("state = %+v, want the block-descriptions judgment", step)
	}
	answerFile := step.Judgment.AnswerFile
	if want := filepath.Join(testArtifactDir(t, cfg, day), "descriptions.json"); answerFile != want {
		t.Fatalf("answer file = %q, want %q", answerFile, want)
	}
	if step.Judgment.DraftDigest == "" {
		t.Fatal("the descriptions judgment carries no draft digest")
	}
	writeNextDescriptions(t, filepath.Join(testArtifactDir(t, cfg, day), "draft.json"), answerFile, step.Judgment.DraftDigest)

	step = nextDoc(t, args...)
	if step.State != stateAction || step.Step != "record" {
		t.Fatalf("state/step = %q/%q, want action/record", step.State, step.Step)
	}
	joined := strings.Join(step.Action.Cmd, " ")
	if !strings.Contains(joined, "--draft "+filepath.Join(testArtifactDir(t, cfg, day), "draft.json")) ||
		!strings.Contains(joined, "--descriptions "+answerFile) {
		t.Fatalf("record argv = %#v, want the derived draft and answer paths", step.Action.Cmd)
	}
	executable(t, step)

	step = nextDoc(t, args...)
	if step.State != stateAction || step.Step != "stage" {
		t.Fatalf("state/step = %q/%q, want action/stage", step.State, step.Step)
	}
	if strings.Contains(strings.Join(step.Action.Cmd, " "), "--project") {
		t.Fatalf("stage argv = %#v, want no --project", step.Action.Cmd)
	}
	executable(t, step)

	step = nextDoc(t, args...)
	if step.State != stateAction || step.Step != "save" {
		t.Fatalf("state/step = %q/%q, want action/save", step.State, step.Step)
	}
	executable(t, step)

	step = nextDoc(t, args...)
	if step.State != stateStopped || !strings.Contains(step.Reason, "gradion") {
		t.Fatalf("state/reason = %q/%q, want stopped naming gradion", step.State, step.Reason)
	}
	if step.Action != nil {
		t.Fatalf("stopped state carries an action: %+v", step.Action)
	}

	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	state := store.State()
	state.Plans = map[string]*timesheet.PlanRecord{
		"plan-ready": {
			PlanID: "plan-ready", Date: day, State: timesheet.PlanReady,
			CreatedAt:       time.Now().UTC().Format(time.RFC3339),
			SourceRevisions: map[string]int{day: state.Days[day].Revision},
		},
	}
	if err := store.CommitState(state); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	_ = store.Close()

	step = nextDoc(t, args...)
	if step.State != stateHumanGates || step.Human == nil {
		t.Fatalf("state = %+v, want human_gates", step)
	}
	for _, want := range []string{" submit ", day} {
		if !strings.Contains(step.Human.ApproveCommand+" "+step.Human.SubmitCommand, want) {
			t.Fatalf("human commands %q / %q do not carry %q", step.Human.ApproveCommand, step.Human.SubmitCommand, want)
		}
	}
	if !strings.Contains(step.Human.SubmitCommand, "--approval-id '<approval_id>'") {
		t.Fatalf("submit command = %q, want the approval-id placeholder", step.Human.SubmitCommand)
	}
	if step.Action != nil || step.Judgment != nil {
		t.Fatalf("human_gates carries an action or judgment: %+v", step)
	}
}

func TestNextClassificationContentBinding(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfigWithGradion(t, "http://127.0.0.1:1/mcp")
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
		timedBlock("work-2", "11:00", "12:00", "Review the deck"),
	})
	seedCatalog(t, cfg, "cat-1",
		timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"},
		timesheet.ClassificationOption{ID: "cls-ops", Name: "Operations", Code: "OPS"},
	)
	args := []string{"--config", cfg, "--date", testDate}

	step := nextDoc(t, args...)
	if step.State != stateAction || step.Step != "stage" {
		t.Fatalf("state/step = %q/%q, want action/stage", step.State, step.Step)
	}
	executable(t, step)

	step = nextJudgmentDoc(t, cfg, "cat-1", args...)
	if step.State != stateJudgment || step.Judgment == nil || step.Judgment.Kind != judgmentClassifications {
		t.Fatalf("state = %+v, want the classification judgment", step)
	}
	if len(step.Judgment.Blocks) != 2 || step.Judgment.LedgerDigest == "" {
		t.Fatalf("judgment = %+v, want two current blocks and a ledger binding", step.Judgment)
	}
	ids := map[string]string{}
	for _, b := range step.Judgment.Blocks {
		ids[b.BlockID] = b.IntervalID
		if b.Description == "" || b.Start == "" || b.End == "" {
			t.Fatalf("block = %+v, want the recorded content so no draft artifact is needed", b)
		}
	}
	oldDigest := step.Judgment.LedgerDigest
	answerFile := step.Judgment.AnswerFile

	writeAnswerFile(t, answerFile, `{"selections": {"`+ids["work-1"]+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`)
	step = nextDoc(t, args...)
	if step.State != stateJudgment {
		t.Fatalf("state = %q for an unbound choices file, want the judgment again", step.State)
	}
	if step.Action != nil {
		t.Fatalf("a stale choices file derived the action %#v", step.Action.Cmd)
	}

	writeAnswerFile(t, answerFile, `{"ledger_digest": "`+oldDigest+`", "selections": {`+
		`"`+ids["work-1"]+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"},`+
		`"`+ids["work-2"]+`": {"catalog_id": "cat-1", "classification_id": "cls-ops"}}}`)
	step = nextDoc(t, args...)
	if step.State != stateAction || step.Step != "classify" {
		t.Fatalf("state/step = %q/%q, want action/classify for the bound answer", step.State, step.Step)
	}
	executable(t, step)

	if step = nextDoc(t, args...); step.Step != "save" {
		t.Fatalf("step = %q, want save once the sidecar binds", step.Step)
	}

	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-1", Date: testDate, Start: "09:00", End: "10:00", Description: "Build the bridge, edited", Kind: "work"},
		{BlockID: "work-2", Date: testDate, Start: "11:00", End: "12:00", Description: "Review the deck", Kind: "work"},
	})
	step = nextJudgmentDoc(t, cfg, "cat-1", args...)
	if step.State != stateJudgment || step.Step != "classify" {
		t.Fatalf("state/step = %q/%q, want a fresh classification judgment after a content edit", step.State, step.Step)
	}
	if step.Judgment.LedgerDigest == oldDigest {
		t.Fatal("the ledger digest did not change although a block description did")
	}
	if got := step.Judgment.Blocks[0].Description; got != "Build the bridge, edited" {
		t.Fatalf("block description = %q, want the edited content reported", got)
	}
	if step.Action != nil {
		t.Fatalf("the stale choices file derived the action %#v", step.Action.Cmd)
	}

	newIDs := map[string]string{}
	for _, b := range step.Judgment.Blocks {
		newIDs[b.BlockID] = b.IntervalID
	}
	writeAnswerFile(t, answerFile, `{"ledger_digest": "`+step.Judgment.LedgerDigest+`", "selections": {`+
		`"`+newIDs["work-1"]+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"},`+
		`"`+newIDs["work-2"]+`": {"catalog_id": "cat-1", "classification_id": "cls-ops"}}}`)
	step = nextDoc(t, args...)
	if step.State != stateAction || step.Step != "classify" {
		t.Fatalf("state/step = %q/%q, want action/classify for the new bound answer", step.State, step.Step)
	}
	executable(t, step)
	step = nextDoc(t, args...)
	if step.State != stateAction || step.Step != "save" {
		t.Fatalf("state/step = %q/%q, want action/save after the new classification", step.State, step.Step)
	}
	executable(t, step)

	confirmed := readDayConfirmed(t, cfg, testDate)
	got := map[string]string{}
	for _, iv := range confirmed {
		got[iv.Description] = iv.RemoteClassificationID
	}
	if got["Build the bridge, edited"] != "cls-dev" || got["Review the deck"] != "cls-ops" {
		t.Fatalf("durable classifications = %+v, want the newly authored selections", got)
	}

	step = nextDoc(t, args...)
	if step.State != stateAction || step.Step != "plan" {
		t.Fatalf("state/step = %q/%q, want action/plan after save", step.State, step.Step)
	}

	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-1", Date: testDate, Start: "09:00", End: "10:00", Description: "Changed after save", Kind: "work"},
		{BlockID: "work-2", Date: testDate, Start: "11:00", End: "12:00", Description: "Review the deck", Kind: "work"},
	})
	step = nextDoc(t, args...)
	if step.State != stateStopped || step.Human != nil {
		t.Fatalf("state/human = %q/%+v, want stopped with no human gates for changed content", step.State, step.Human)
	}
	if !strings.Contains(step.Reason, "stage --revise --date "+testDate) {
		t.Fatalf("reason = %q, want the stage --revise remedy", step.Reason)
	}
}

func TestNextWithoutDateResolvesTodayInConfiguredTimezone(t *testing.T) {
	pinClock(t)
	t.Setenv("TMPDIR", t.TempDir())
	cfg, _ := writeSyntheticConfig(t, "Asia/Ho_Chi_Minh")

	step := nextDoc(t, "--config", cfg)
	if step.Day != "2026-10-27" {
		t.Fatalf("day = %q, want the pinned Asia/Ho_Chi_Minh day", step.Day)
	}
	if !strings.Contains(strings.Join(step.Action.Cmd, " "), "--date 2026-10-27") {
		t.Fatalf("derived argv = %#v, want the resolved day pinned explicitly", step.Action.Cmd)
	}
	if err := runNext([]string{"--config", cfg, "--date", "20-10-2026"}); err == nil ||
		!strings.Contains(err.Error(), "next: --date must be YYYY-MM-DD") {
		t.Fatalf("bad date = %v, want next's format error", err)
	}
}

func TestNextDerivedArgvIsAbsoluteFromAnyCwd(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfig(t)
	absCfg, err := filepath.Abs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	t.Chdir(work)
	relative, err := filepath.Rel(work, absCfg)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(relative) {
		t.Fatalf("fixture is not cross-directory: %q", relative)
	}
	step := nextDoc(t, "--config", relative, "--date", nextDay)
	if step.Action.Cmd[3] != absCfg {
		t.Fatalf("derived argv = %#v, want the absolute config path %q", step.Action.Cmd, absCfg)
	}
	if step.Action.Cmd[1] != "run" {
		t.Fatalf("derived verb = %q, want run", step.Action.Cmd[1])
	}
	executable(t, step)
}

func TestNextReadOnlyDerivationsAreRepeatable(t *testing.T) {
	cfg := writeTestConfig(t)
	first := nextDoc(t, "--config", cfg, "--date", nextDay)
	second := nextDoc(t, "--config", cfg, "--date", nextDay)
	if first.State != second.State || first.Step != second.Step ||
		strings.Join(first.Action.Cmd, " ") != strings.Join(second.Action.Cmd, " ") {
		t.Fatalf("derivations differ:\n%+v\n%+v", first, second)
	}
}

func TestNextContentProofMirrorsTheStageVerb(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Short block"),
		timedBlock("work-2", "11:00", "16:00", "Five-hour block"),
		{DraftBlock: timesheet.DraftBlock{BlockID: "day-1", Kind: "day"}, Description: "Whole-day entry with no anchors"},
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if receipt.StagedIntervals != 3 {
		t.Fatalf("staged_intervals = %d, want 1 + 2 chunks", receipt.StagedIntervals)
	}

	artifacts, err := timesheet.ReadDayArtifacts(statePathFor(cfg), testDate)
	if err != nil {
		t.Fatalf("artifacts: %v", err)
	}
	units := ledgerUnits(artifacts.Entries.Entries, testDate, "UTC")
	if len(units) != 2 {
		t.Fatalf("units = %+v, want two stageable units (the anchorless entry is not one)", units)
	}
	if !stageMapMatchesUnits(artifacts.StageMap, units) {
		t.Fatalf("the projection does not match the stage verb's own map: %+v vs %+v", units, artifacts.StageMap)
	}
	blocks := stagedBlocksFrom(units, artifacts.StageMap)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v, want every staged interval id bound", blocks)
	}
	for _, ref := range receipt.Staged {
		if _, ok := blocks[ref.IntervalID]; !ok {
			t.Fatalf("receipt interval %s is not bound by the projection", ref.IntervalID)
		}
	}
	for _, e := range blocks {
		if e.BlockID == "day-1" {
			t.Fatalf("the anchorless entry produced an interval: %+v", e)
		}
	}

	stale := append([]ledgerUnit(nil), units...)
	stale[1].Spans = stale[1].Spans[:1]
	if stageMapMatchesUnits(artifacts.StageMap, stale) {
		t.Fatal("the proof accepted a stale span count")
	}

	if _, _, err := captureOutput(t, func() error {
		return runSave([]string{"--config", cfg, "--date", testDate})
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	confirmed := readDayConfirmed(t, cfg, testDate)
	if len(confirmed) != 3 {
		t.Fatalf("confirmed = %+v, want three saved intervals", confirmed)
	}
	if !sameContent(ledgerContentSpans(units), confirmedContentSpans(confirmed)) {
		t.Fatalf("ledger projection %+v does not match the saved content %+v",
			ledgerContentSpans(units), confirmedContentSpans(confirmed))
	}
	step := nextDoc(t, "--config", cfg, "--date", testDate)
	if step.State != stateStopped || !strings.Contains(step.Reason, "gradion") {
		t.Fatalf("state/reason = %q/%q, want the plan tail (gradion), not a false drift stop", step.State, step.Reason)
	}
}

func TestNextStaleClassificationWithoutCatalog(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
	})
	args := []string{"--config", cfg, "--date", testDate}

	step := nextDoc(t, args...)
	if step.State != stateAction || step.Step != "stage" {
		t.Fatalf("state/step = %q/%q, want action/stage", step.State, step.Step)
	}
	executable(t, step)

	if err := timesheet.WriteClassificationChoices(statePathFor(cfg), timesheet.AppVersion, testDate,
		map[string]timesheet.ClassificationChoiceEntry{
			"iv-1": {CatalogID: "cat-1", ClassificationID: "cls-1", ContentDigest: "digest-of-earlier-content"},
		}, time.Now()); err != nil {
		t.Fatalf("seed stale sidecar: %v", err)
	}

	step = nextDoc(t, args...)
	if step.State != stateStopped || step.Action != nil {
		t.Fatalf("state/action = %q/%+v, want stopped without an action", step.State, step.Action)
	}
	for _, want := range []string{"gradion.enabled", "clear-classifications.json", "will never clear"} {
		if !strings.Contains(step.Reason, want) {
			t.Fatalf("reason = %q, want %q named", step.Reason, want)
		}
	}

	clearFile := filepath.Join(testArtifactDir(t, cfg, testDate), "clear-classifications.json")
	if !strings.Contains(step.Reason, clearFile) {
		t.Fatalf("reason = %q, want the exact clear file %q", step.Reason, clearFile)
	}
	writeAnswerFile(t, clearFile, `{"selections":{}}`)
	if _, _, err := captureOutput(t, func() error {
		return runClassify([]string{"--config", cfg, "--date", testDate, "--file", clearFile})
	}); err != nil {
		t.Fatalf("the offline clear remedy failed: %v", err)
	}

	step = nextDoc(t, args...)
	if step.State != stateAction || step.Step != "save" {
		t.Fatalf("state/step = %q/%q, want action/save after the explicit clear", step.State, step.Step)
	}
}

func TestNextCorruptDraftIsRecollected(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	cfg := writeRunConfig(t)
	srv, _ := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)
	day := "2026-10-20"
	args := []string{"--config", cfg, "--env-file", envFile, "--date", day}
	draftPath := filepath.Join(testArtifactDir(t, cfg, day), "draft.json")
	if err := os.MkdirAll(filepath.Dir(draftPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(draftPath, []byte("{not the draft schema"), 0o600); err != nil {
		t.Fatal(err)
	}

	step := nextDoc(t, args...)
	if step.State != stateAction || step.Step != "run" {
		t.Fatalf("state/step = %q/%q, want action/run for a corrupt draft", step.State, step.Step)
	}
	if !strings.Contains(step.Action.Why, "not a valid draft") {
		t.Fatalf("why = %q, want the corrupt-draft predicate named", step.Action.Why)
	}
	if step.Judgment != nil {
		t.Fatal("a corrupt draft produced a judgment")
	}

	executable(t, step)
	step = nextDoc(t, args...)
	if step.State != stateJudgment || step.Judgment == nil {
		t.Fatalf("state = %+v, want the descriptions judgment for the rewritten draft", step)
	}
}

func TestNextUnderivableStateIsTypedStopped(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfig(t)
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	state := store.State()
	state.Plans = map[string]*timesheet.PlanRecord{
		"plan-x": {PlanID: "plan-x", Date: nextDay, State: timesheet.PlanReady, CreatedAt: "not-a-time"},
	}
	if err := store.CommitState(state); err != nil {
		t.Fatalf("seed unreadable plan: %v", err)
	}
	_ = store.Close()

	out, _, err := captureOutput(t, func() error {
		return runNext([]string{"--config", cfg, "--date", nextDay})
	})
	if err != nil {
		t.Fatalf("next returned a process error %v, want a typed stopped document", err)
	}
	var n nextAction
	if err := json.Unmarshal(out, &n); err != nil {
		t.Fatalf("next did not print one typed JSON action: %v\n%s", err, out)
	}
	if n.State != stateStopped || !strings.Contains(n.Reason, "created_at") {
		t.Fatalf("state/reason = %q/%q, want stopped naming the unreadable fact", n.State, n.Reason)
	}
}

func TestNextStoreLockedStaysAnError(t *testing.T) {
	cfg := writeTestConfig(t)
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()
	err = runNext([]string{"--config", cfg, "--date", nextDay})
	if err == nil || !strings.Contains(err.Error(), "locked by another running process") {
		t.Fatalf("next under a held lock = %v, want the store_locked error", err)
	}
}

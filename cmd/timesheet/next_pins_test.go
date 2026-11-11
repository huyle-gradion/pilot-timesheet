package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestShellJoinQuotesEveryShellMetacharacter(t *testing.T) {
	for _, c := range []string{" ", ";", "'", "$", "`", "|", "&", "*", `"`, "\n", "(", "<"} {
		if got := shellJoin([]string{"a" + c + "b"}); !strings.HasPrefix(got, "'") {
			t.Errorf("shellJoin(%q) = %s, want it quoted", "a"+c+"b", got)
		}
	}
	if got := shellJoin([]string{"/exe", "--date", "2026-10-27"}); got != "/exe --date 2026-10-27" {
		t.Fatalf("plain argv = %s", got)
	}
}

func TestTimestampAfterComparesInstantsAcrossOffsets(t *testing.T) {
	after, err := timestampAfter("2026-01-01T09:00:00+07:00", "2026-01-01T03:00:00Z")
	if err != nil || after {
		t.Fatalf("02:00Z after 03:00Z = %v, %v; want false", after, err)
	}
}

func TestCarryOutCurrentLegacyDayCarriesNothing(t *testing.T) {
	seg := []timesheet.CarrySegment{{SourceDate: "2026-01-01", Date: "2026-01-02", Start: "00:00", End: "01:00", Minutes: 60}}
	if carryOutCurrent(false, seg, seg) {
		t.Fatal("a day saved under a legacy rule passed with a carry record")
	}
	if !carryOutCurrent(false, seg, nil) || !carryOutCurrent(true, seg, seg) {
		t.Fatal("a matching carry record was refused")
	}
}

func TestDraftDigestChangesWithDraftContent(t *testing.T) {
	if draftDigest([]byte(`{"blocks":[1]}`)) == draftDigest([]byte(`{"blocks":[2]}`)) {
		t.Fatal("two different drafts share a digest")
	}
}

func TestAdvanceDayStopsARepeatedJudgmentWithoutProgress(t *testing.T) {
	obs := nextObservation{Day: nextDay, DraftPresent: true, DraftValid: true, DraftBlocks: 1, DraftDigest: "d1"}
	calls := 0
	d := actDispatcher{day: nextDay, judge: func(*nextJudgment) (judgeReceipt, error) {
		calls++
		return judgeReceipt{}, nil
	}}
	derive := func() (nextObservation, *nextAction, error) { return obs, nil, nil }
	rep, err := advanceDay(derive, d, advanceLimits{MaxActions: 5})
	if err != nil || calls != 1 || rep.State != stateStopped || !strings.Contains(rep.Reason, "asked again") {
		t.Fatalf("report = %+v, %v after %d judge call(s); want one call then a judgment_failed stop", rep.nextAction, err, calls)
	}
}

func TestReadJudgmentFailureTreatsAnUnreadableRecordAsNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), judgmentFailuresFile)
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := readJudgmentFailure(path); f != nil || err != nil {
		t.Fatalf("record = %+v, %v; want none", f, err)
	}
}

func TestRecordJudgmentFailureResetsForAnotherKind(t *testing.T) {
	cfg := writeRunConfig(t)
	c, err := timesheet.LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := testArtifactDir(t, cfg, nextDay)
	writeAnswerFile(t, filepath.Join(dir, judgmentFailuresFile),
		`{"kind":"classifications","digest":"d1","answer_sha":"x","failures":1,"last_error":"e"}`)
	o := nextObservation{Day: nextDay, DescriptionsPath: filepath.Join(dir, "descriptions.json"), DescriptionsBinding: "d1"}
	writeAnswerFile(t, o.DescriptionsPath, `{"draft_digest":"d1"}`)
	act := nextAction{Day: nextDay}.withAction("record", []string{"/exe", "record"}, "why")
	bad := &timesheet.AppError{Code: timesheet.CodeBadRequest, Message: "record: missing"}
	if err := recordJudgmentFailure(c, nextDay, o, act, bad); err != nil {
		t.Fatal(err)
	}
	if f, _ := readJudgmentFailure(filepath.Join(dir, judgmentFailuresFile)); f == nil || f.Failures != 1 || f.Kind != judgmentBlockDescriptions {
		t.Fatalf("record = %+v, want a fresh count for the other kind", f)
	}
}

func TestMergeHistoryKeepsTheAnswersOwnPick(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	dir, err := dayArtifactsDir(statePath, nextDay)
	if err != nil {
		t.Fatal(err)
	}
	writeAnswerFile(t, filepath.Join(dir, historyFileName),
		`{"ledger_digest":"L","selections":{"iv-1":{"catalog_id":"cat-h","classification_id":"cls-h"},"iv-2":{"catalog_id":"cat-h","classification_id":"cls-h"}}}`)
	sel := map[string]timesheet.ClassificationChoiceEntry{"iv-1": {CatalogID: "cat-j", ClassificationID: "cls-j"}}
	mergeHistorySelections(statePath, nextDay, []byte(`{"ledger_digest":"L"}`), sel)
	if sel["iv-1"].ClassificationID != "cls-j" || sel["iv-1"].Source != timesheet.SelectionSourceJudgment {
		t.Fatalf("iv-1 = %+v, want the answer's own pick", sel["iv-1"])
	}
	if sel["iv-2"].ClassificationID != "cls-h" || sel["iv-2"].Source != timesheet.SelectionSourceHistory {
		t.Fatalf("iv-2 = %+v, want the history pick", sel["iv-2"])
	}
}

func TestCarryFollowUpNeverVisitsAFutureDate(t *testing.T) {
	always := func(string) bool { return true }
	if next, ok := carryFollowUp("2026-10-27", "2026-10-27", true, []string{"2026-10-27"}, always); ok {
		t.Fatalf("carry follow-up = %s, want none after today", next)
	}
}

func TestDailyTodayReportsTheLogAfterCutoff(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	policy := (&timesheet.Config{}).EffectiveDaily()
	for at, want := range map[string]bool{"2026-10-27 09:00": false, "2026-10-27 23:00": true} {
		now, _ := time.ParseInLocation("2006-01-02 15:04", at, loc)
		if day, after, err := dailyToday("Asia/Ho_Chi_Minh", policy, now); err != nil || day != "2026-10-27" || after != want {
			t.Errorf("%s: %s %v %v; want after-cutoff %v", at, day, after, err, want)
		}
	}
}

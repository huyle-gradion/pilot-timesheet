package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func incompleteSourceDay(t *testing.T) string {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfigWithGithub(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Bridge work PRs: #12 acme/api"),
	})
	seedEvidence(t, cfg, testDate, &timesheet.DayEvidence{
		Attempts: []timesheet.SourceAttempt{
			{Source: timesheet.SourceGithub, CollectionID: "col-gate-1", AttemptedAt: "2026-10-26T10:00:00Z", Status: timesheet.StatusIncomplete},
		},
	})
	return cfg
}

func TestNextDoPerformsTheHumansAcknowledgementThenContinues(t *testing.T) {
	cfg := incompleteSourceDay(t)
	args := []string{"--config", cfg, "--date", testDate, "--do"}
	rep, err := advanceDoc(t, args...)
	if err != nil {
		t.Fatalf("next --do: %v", err)
	}
	q := rep.Ask
	if rep.State != stateAskHuman || q == nil || q.Kind != askSourceAck {
		t.Fatalf("report = %+v, want the acknowledgement question", rep.nextAction)
	}

	body, _ := json.Marshal(map[string]string{"digest": q.Digest, "answer": "sure", "surface": "chat"})
	writeAnswerFile(t, q.AnswerFile, string(body))
	rep, _ = advanceDoc(t, args...)
	if rep.State != stateAskHuman || !strings.Contains(rep.Ask.LastError, "not one of") || len(rep.Ran) != 0 {
		t.Fatalf("off-menu answer = %+v ran %+v", rep.nextAction, rep.Ran)
	}

	body, _ = json.Marshal(map[string]string{"digest": q.Digest, "answer": "acknowledge", "surface": "chat"})
	writeAnswerFile(t, q.AnswerFile, string(body))
	rep, err = advanceDoc(t, args...)
	if err != nil {
		t.Fatalf("next --do after the answer: %v (%+v)", err, rep.nextAction)
	}
	acked := false
	for _, r := range rep.Ran {
		if r.Step == "acknowledge" && r.Result == "ok" {
			acked = true
		}
	}
	if !acked || rep.State == stateAskHuman {
		t.Fatalf("ran = %+v state %q, want acknowledge ok and the gate passed", rep.Ran, rep.State)
	}
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if !store.Evidence(testDate).Attempts[0].Acknowledged {
		t.Fatal("the answered acknowledgement was not recorded in the store")
	}
}

func TestADayWithoutHumanDecisionsNeverAsks(t *testing.T) {
	for _, o := range []nextObservation{unclassifiedDay(), recordedDay(), finalizedDay()} {
		if n := deriveDayNext(o); n.State == stateAskHuman || n.Ask != nil {
			t.Fatalf("derivation = %+v, want no human question", n)
		}
	}
}

func TestTuiRendersTheSameAsk(t *testing.T) {
	o := recordedDay()
	o.Workflow.Sources = []timesheet.SourceSnapshot{{
		Source: timesheet.SourceGithub, Status: timesheet.StatusIncomplete, CollectionID: "col-1", Attempted: true,
	}}
	a := deriveDayNext(o)
	var b strings.Builder
	renderAsk(&b, a.Ask, testStyles())
	out := b.String()
	for _, want := range []string{askSourceAck, "acknowledge", a.Ask.AnswerFile, a.Ask.Digest} {
		if !strings.Contains(out, want) {
			t.Fatalf("tui ask render lacks %q:\n%s", want, out)
		}
	}
}

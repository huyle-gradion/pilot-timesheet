package timesheet

import (
	"reflect"
	"testing"
)

func TestSubmittedSelectionsKeepsNewestConfirmedObservedClassification(t *testing.T) {
	intent := func(date, local, status, at, planned, observed string) WriteIntent {
		in := WriteIntent{Date: date, LocalID: local, Status: status, RecordedAt: at,
			Operation: PlanOperation{ClassificationID: planned}}
		if observed != "" {
			in.Observed = &RemoteEntry{ClassificationID: observed}
		}
		return in
	}
	state := &State{Intents: []WriteIntent{
		intent("2026-11-02", "iv-a", IntentConfirmed, "2026-11-02T10:00:00Z", "old", ""),
		intent("2026-11-02", "iv-a", IntentConfirmed, "2026-11-03T10:00:00Z", "planned", "observed"),
		intent("2026-11-02", "iv-a", IntentConfirmed, "2026-11-02T09:00:00Z", "older", ""),
		intent("2026-11-02", "iv-b", IntentUncertain, "2026-11-02T10:00:00Z", "unsubmitted", ""),
		intent("2026-11-04", "iv-c", IntentConfirmed, "2026-11-04T10:00:00Z", "excluded", ""),
		intent("2026-11-01", "iv-d", IntentConfirmed, "2026-11-01T10:00:00Z", "planned-d", ""),
	}}
	got := submittedSelections(state, "2026-11-04")
	want := []SubmittedSelection{
		{Date: "2026-11-01", IntervalID: "iv-d", ClassificationID: "planned-d"},
		{Date: "2026-11-02", IntervalID: "iv-a", ClassificationID: "observed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("submittedSelections = %+v, want %+v", got, want)
	}
}

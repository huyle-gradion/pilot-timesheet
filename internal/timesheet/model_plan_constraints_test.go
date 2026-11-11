package timesheet

import (
	"encoding/json"
	"testing"
)

func TestDaySessionReviewStateIsNeverSerialized(t *testing.T) {
	day := &Day{
		Date: "2026-10-20", Timezone: "UTC", Revision: 1,
		Staged:        []Interval{{ID: "iv-staged", Date: "2026-10-20", Start: "09:00", End: "10:00", Description: "staged only"}},
		SavedRevision: 7,
	}
	data, err := json.Marshal(day)
	if err != nil {
		t.Fatal(err)
	}
	var back Day
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Staged != nil || back.SavedRevision != 0 {
		t.Fatalf("session-only review state reached the durable encoding: %s", data)
	}
}

func TestReconcileAndPreviewCallOnlyReadTools(t *testing.T) {
	s, fake, _, date := classifiedFixture(t, "2026-10-20 09:00-10:00 Synthetic work")
	fake.mu.Lock()
	fake.calls = nil
	fake.mu.Unlock()
	if _, err := s.reconcileDate(date); err != nil {
		t.Fatal(err)
	}
	if _, err := s.previewPlan(date); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) == 0 {
		t.Fatal("no remote read was observed")
	}
	for _, name := range fake.calls {
		if name != toolGetMyWeek && name != toolGetClassifications {
			t.Fatalf("reconcile or preview called a non-read tool %q", name)
		}
	}
}

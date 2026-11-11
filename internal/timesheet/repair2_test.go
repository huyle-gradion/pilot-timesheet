package timesheet

import (
	"errors"
	"path/filepath"
	"testing"
)

func testEvidenceRecord() *DayEvidence {
	return &DayEvidence{
		Attempts: []SourceAttempt{{
			Source:       SourceGithub,
			CollectionID: "col-test-1",
			AttemptedAt:  "2026-10-22T00:00:00Z",
			Status:       StatusIncomplete,
		}},
	}
}

func TestCommitEvidenceRollbackOfAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	restore := storeFaultHook
	storeFaultHook = func(stage string) error {
		if stage == "before_rename" {
			return errors.New("synthetic activation failure")
		}
		return nil
	}
	err = store.CommitEvidence("2026-10-22", testEvidenceRecord())
	storeFaultHook = restore
	if err == nil {
		t.Fatal("expected a write failure from the fault hook")
	}
	if ev := store.Evidence("2026-10-22"); ev != nil {
		t.Fatalf("failed first collect left evidence state behind: %+v", ev)
	}

	if err := store.CommitEvidence("2026-10-24", testEvidenceRecord()); err != nil {
		t.Fatalf("CommitEvidence after restore: %v", err)
	}
	if ev := store.Evidence("2026-10-22"); ev != nil {
		t.Fatalf("absent date materialized in memory: %+v", ev)
	}
	if err := store.CommitEvidence("2026-10-22", testEvidenceRecord()); err != nil {
		t.Fatalf("CommitEvidence on absent date after restore: %v", err)
	}
	if store.Uncertain() {
		t.Fatal("pre-activation failure must not leave the session uncertain")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reload after failed-then-successful evidence writes: %v", err)
	}
	defer reopened.Close()
	if reopened.Evidence("2026-10-22") == nil || reopened.Evidence("2026-10-24") == nil {
		t.Fatal("reload lost committed evidence records")
	}
}

func TestCommitEvidenceStickyPostRenameUncertainty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.CommitEvidence("2026-10-22", testEvidenceRecord()); err != nil {
		t.Fatalf("initial CommitEvidence: %v", err)
	}

	restore := storeFaultHook
	storeFaultHook = func(stage string) error {
		if stage == "directory_sync" {
			return errors.New("synthetic post-rename durability failure")
		}
		return nil
	}
	err = store.CommitEvidence("2026-10-24", testEvidenceRecord())
	storeFaultHook = restore
	if err == nil {
		t.Fatal("expected a post-rename uncertain failure")
	}
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Code != CodeWriteUncertain {
		t.Fatalf("expected %s, got %v", CodeWriteUncertain, err)
	}
	if !store.Uncertain() {
		t.Fatal("post-rename failure must leave the session uncertain")
	}
	if err := store.CommitEvidence("2026-10-26", testEvidenceRecord()); err == nil {
		t.Fatal("further evidence writes must stay blocked while uncertain")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("activated state must load after uncertain close: %v", err)
	}
	defer reopened.Close()
	if reopened.Evidence("2026-10-24") == nil {
		t.Fatal("activated evidence record missing after reload")
	}
}

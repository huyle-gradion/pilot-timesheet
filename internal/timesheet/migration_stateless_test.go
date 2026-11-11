package timesheet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func openFromDisk(t *testing.T, cfgPath, envPath string) (*Session, func()) {
	t.Helper()
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	fileEnv, err := LoadEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	return NewSession(cfg, MergeEnv(fileEnv), store), func() { _ = store.Close() }
}

func decodeResult(t *testing.T, resp Response, v any) {
	t.Helper()
	requireOK(t, resp)
	data, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationApplyIsStatelessAcrossProcesses(t *testing.T) {
	fixture, _, dir := migrationFixture(t)
	cfgBytes, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "timesheet.json")
	envPath := filepath.Join(dir, ".env.local")
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dates := []string{"2026-10-20"}

	s, done := openFromDisk(t, cfgPath, envPath)
	if resp, _ := s.HandleLine([]byte("{not json")); resp.OK || resp.Error == nil || resp.Error.Code != CodeBadRequest {
		t.Fatalf("malformed line: %+v", resp)
	}
	requireCode(t, call(t, s, Request{ID: "x1", Op: "no_such_op"}), CodeUnknownOp)
	var preview MigrationPreview
	decodeResult(t, call(t, s, Request{ID: "m1", Op: "migration_preview", Dates: dates}), &preview)
	if !preview.Ready || preview.MigrationID == "" || preview.NoChange {
		t.Fatalf("preview not ready: %+v", preview)
	}
	done()
	before, err := os.ReadFile(fixture.StatePath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	s, done = openFromDisk(t, cfgPath, envPath)
	requireCode(t, call(t, s, Request{ID: "m2", Op: "migration_apply", Dates: dates, MigrationID: "bogus"}), CodeMigrationStale)
	requireCode(t, call(t, s, Request{ID: "m3", Op: "migration_apply", Dates: []string{"2026-10-20", "2026-10-21"}, MigrationID: preview.MigrationID}), CodeMigrationStale)
	requireCode(t, call(t, s, Request{ID: "m4", Op: "migration_apply", MigrationID: preview.MigrationID}), CodeMigrationInvalid)
	after, err := os.ReadFile(fixture.StatePath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused apply changed the store")
	}
	var receipt MigrationApplyReceipt
	decodeResult(t, call(t, s, Request{ID: "m5", Op: "migration_apply", Dates: dates, MigrationID: preview.MigrationID}), &receipt)
	if receipt.Status != MigrationApplied || receipt.ReceiptID != preview.MigrationID {
		t.Fatalf("apply receipt: %+v", receipt)
	}
	var status StatusResult
	decodeResult(t, call(t, s, Request{ID: "h1", Op: "status"}), &status)
	if status.AppVersion != AppVersion || status.MigrationReceipts != 1 {
		t.Fatalf("status after apply: %+v", status)
	}
	requireCode(t, call(t, s, Request{ID: "m6", Op: "migration_apply", Dates: dates, MigrationID: preview.MigrationID}), CodeMigrationStale)
	done()

	s, done = openFromDisk(t, cfgPath, envPath)
	defer done()
	var again MigrationPreview
	decodeResult(t, call(t, s, Request{ID: "m7", Op: "migration_preview", Dates: dates}), &again)
	if !again.NoChange || again.ExistingReceipt != preview.MigrationID {
		t.Fatalf("repeat preview: %+v", again)
	}
	var noop MigrationApplyReceipt
	decodeResult(t, call(t, s, Request{ID: "m8", Op: "migration_apply", Dates: dates, MigrationID: again.MigrationID}), &noop)
	if noop.Status != MigrationNoChange || noop.ReceiptID != preview.MigrationID {
		t.Fatalf("repeat apply: %+v", noop)
	}
}

func TestMigrationApplyIsStaleWhenTheConfigChanged(t *testing.T) {
	cfg, _, _ := migrationFixture(t)
	dates := []string{"2026-10-20"}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var preview MigrationPreview
	decodeResult(t, call(t, NewSession(cfg, map[string]string{}, store), Request{ID: "p", Op: "migration_preview", Dates: dates}), &preview)
	if !preview.Ready {
		t.Fatalf("preview not ready: %+v", preview)
	}

	edited := *cfg
	mig := *cfg.Migration
	mig.ClassificationMap = map[string]string{"0f8fad5b-d9cb-469f-a165-70867728950e": "DEV"}
	edited.Migration = &mig
	s := NewSession(&edited, map[string]string{}, store)
	requireCode(t, call(t, s, Request{ID: "a", Op: "migration_apply", Dates: dates, MigrationID: preview.MigrationID}), CodeMigrationStale)
	if st := store.State(); len(st.Days) != 0 || st.Migration != nil && len(st.Migration.Receipts) != 0 {
		t.Fatal("a stale apply wrote the store")
	}

	var fresh MigrationPreview
	decodeResult(t, call(t, s, Request{ID: "p2", Op: "migration_preview", Dates: dates}), &fresh)
	if fresh.MigrationID == preview.MigrationID {
		t.Fatal("a different classification_map previewed the same migration_id")
	}
	var receipt MigrationApplyReceipt
	decodeResult(t, call(t, s, Request{ID: "a2", Op: "migration_apply", Dates: dates, MigrationID: fresh.MigrationID}), &receipt)
	if receipt.Status != MigrationApplied {
		t.Fatalf("apply after a fresh preview: %+v", receipt)
	}
}

package timesheet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// openFromDisk loads config and env file from disk and opens the store, the
// way every stateless command does (issue #272): one fresh session per
// invocation, nothing carried between them but the store.
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

// decodeResult re-encodes a dispatcher result into its typed shape.
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

// TestMigrationApplyIsStatelessAcrossProcesses ports the serve end-to-end
// migration test (#267) to the stateless path that replaced it: preview and
// apply run in SEPARATE sessions over the same store, so the migration_id is
// a content binding rebuilt from the store and the legacy source, never an
// in-process grant. A mismatched id is migration_stale and writes nothing; an
// already-migrated selection is no_change on both preview and apply. The
// dispatcher's envelope behavior (malformed line, unknown op) is pinned here
// too, since the commands and the TUI still drive it in-process.
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

	// Process 1: preview, then exit.
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

	// Process 2: a bogus id and an id for another selection are stale and
	// write nothing; the preview's own id applies with no shared memory.
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
	// The applied id is spent: the target changed, so it no longer rebuilds.
	requireCode(t, call(t, s, Request{ID: "m6", Op: "migration_apply", Dates: dates, MigrationID: preview.MigrationID}), CodeMigrationStale)
	done()

	// Process 3: the already-migrated day is no_change on preview and apply.
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

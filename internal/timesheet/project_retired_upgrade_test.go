package timesheet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PR #292 review: the schema-4 upgrade is one-way, so the activating write
// first keeps a one-time backup of the older file and says so on one line;
// the retired config keys are removed from the file when that is safe; and
// the inert legacy labels never reach review output.

// captureUpgradeNotices collects every UpgradeNotice line for the test.
func captureUpgradeNotices(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	old := UpgradeNotice
	UpgradeNotice = func(line string) { lines = append(lines, line) }
	t.Cleanup(func() { UpgradeNotice = old })
	return &lines
}

func withStoreFault(t *testing.T, stage string) {
	t.Helper()
	old := storeFaultHook
	storeFaultHook = func(s string) error {
		if s == stage {
			return errors.New("synthetic " + stage + " failure")
		}
		return nil
	}
	t.Cleanup(func() { storeFaultHook = old })
}

func TestSchemaUpgradeKeepsAOneTimeBackupAndSaysSo(t *testing.T) {
	path := copyFixtureStore(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	notices := captureUpgradeNotices(t)
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitState(store.State()); err != nil {
		t.Fatal(err)
	}
	backup := path + ".schema3.bak"
	got, err := os.ReadFile(backup)
	if err != nil || string(got) != string(original) {
		t.Fatalf("backup = %v, want the exact schema-3 bytes", err)
	}
	if info, _ := os.Stat(backup); info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, want 0600", info.Mode().Perm())
	}
	if len(*notices) != 1 || !strings.Contains((*notices)[0], backup) || !strings.Contains((*notices)[0], "one-way") ||
		strings.Contains((*notices)[0], "\n") {
		t.Fatalf("notices = %q, want one line naming the backup and the one-way upgrade", *notices)
	}
	// Later writes, in this process or the next, are plain schema-4 writes.
	if err := store.CommitState(store.State()); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.CommitState(reopened.State()); err != nil {
		t.Fatal(err)
	}
	if len(*notices) != 1 {
		t.Fatalf("notices = %q, want only the activating write's line", *notices)
	}
}

func TestSchemaUpgradeNeverOverwritesAnExistingBackup(t *testing.T) {
	path := copyFixtureStore(t)
	backup := path + ".schema3.bak"
	if err := os.WriteFile(backup, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureUpgradeNotices(t)
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.CommitState(store.State()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(backup); string(got) != "keep" {
		t.Fatalf("existing backup overwritten: %q", got)
	}
}

func TestSchemaUpgradeFailedBeforeRenameLeavesNoHalfState(t *testing.T) {
	path := copyFixtureStore(t)
	original, _ := os.ReadFile(path)
	notices := captureUpgradeNotices(t)
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	withStoreFault(t, "before_rename")
	if err := store.CommitState(store.State()); err == nil {
		t.Fatal("the activating write succeeded despite the fault")
	}
	if now, _ := os.ReadFile(path); string(now) != string(original) {
		t.Fatal("a failed activating write changed the schema-3 file")
	}
	if store.DurableSchema() != 3 {
		t.Fatalf("durable schema = %d, want 3", store.DurableSchema())
	}
	if r := store.State().Migration.SchemaUpgrades; len(r) != 1 || r[0].AppliedAt != "" {
		t.Fatalf("pending receipt = %+v, want one unstamped receipt", r)
	}
	if len(*notices) != 0 {
		t.Fatalf("notices = %q, want none for a write that did not activate", *notices)
	}
	storeFaultHook = nil
	if err := store.CommitState(store.State()); err != nil {
		t.Fatal(err)
	}
	if r := store.State().Migration.SchemaUpgrades; len(r) != 1 || r[0].AppliedAt == "" {
		t.Fatalf("receipt after the retry = %+v, want exactly one stamped receipt", r)
	}
}

func TestSchemaUpgradeUncertainAfterRenameKeepsOneReceipt(t *testing.T) {
	path := copyFixtureStore(t)
	captureUpgradeNotices(t)
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	withStoreFault(t, "directory_sync")
	if err := store.CommitState(store.State()); errorCode(err) != CodeWriteUncertain {
		t.Fatalf("activating write = %v, want write_uncertain", err)
	}
	_ = store.Close()
	storeFaultHook = nil
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.DurableSchema() != 4 {
		t.Fatalf("durable schema = %d, want 4", reopened.DurableSchema())
	}
	if r := reopened.State().Migration.SchemaUpgrades; len(r) != 1 || r[0].AppliedAt == "" {
		t.Fatalf("receipts = %+v, want exactly one stamped receipt", r)
	}
}

func TestReviewOmitsLegacyLabels(t *testing.T) {
	path := copyFixtureStore(t)
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	session := NewSession(&Config{Timezone: "Asia/Ho_Chi_Minh", StatePath: path}, map[string]string{}, store)
	for _, op := range []string{"open_day", "review"} {
		resp := call(t, session, Request{ID: op, Op: op, Day: "2026-10-26"})
		requireOK(t, resp)
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		for _, label := range []string{`"project":`, `"classification":`} {
			if strings.Contains(string(data), label) {
				t.Fatalf("%s output carries the legacy label %s: %s", op, label, data)
			}
		}
	}
	// The labels stay in state, inert.
	if iv := store.State().Days["2026-10-26"].Confirmed[0]; iv.Project != "WORK" || iv.Classification != "DEV" {
		t.Fatalf("state lost its legacy labels: %+v", iv)
	}
}

const retiredKeysConfig = `{
  "timezone": "Europe/Amsterdam",
  "state_path": "/tmp/state.json",
  "projects": [
    {"id": "A", "name": "Alpha", "default_classification": "DEV"}
  ],
  "required_env": ["GITHUB_TOKEN"],
  "migration": {"enabled": false, "project_map": {"OLD": "A"}, "classification_map": {"x": "DEV"}},
  "work_hours": {"days": ["mon"], "windows": [{"start": "09:00",   "end": "17:00"}]}
}
`

const retiredKeysRemoved = `{
  "timezone": "Europe/Amsterdam",
  "state_path": "/tmp/state.json",
  "required_env": ["GITHUB_TOKEN"],
  "migration": {"enabled": false, "classification_map": {"x": "DEV"}},
  "work_hours": {"days": ["mon"], "windows": [{"start": "09:00",   "end": "17:00"}]}
}
`

func TestRemoveRetiredConfigKeysKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(retiredKeysConfig), 0o640); err != nil {
		t.Fatal(err)
	}
	removed, backup, err := RemoveRetiredConfigKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(removed, ",") != "projects,migration.project_map" || backup != path+".bak" {
		t.Fatalf("removed = %v, backup = %q", removed, backup)
	}
	if got, _ := os.ReadFile(path); string(got) != retiredKeysRemoved {
		t.Fatalf("rewritten config:\n%s\nwant:\n%s", got, retiredKeysRemoved)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("config mode = %v, want the original 0640", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(backup); string(got) != retiredKeysConfig {
		t.Fatal("backup is not the original bytes")
	}
	if info, _ := os.Stat(backup); info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("rewritten config does not load: %v", err)
	}
	// A second run finds nothing to remove and leaves both files alone.
	removed, _, err = RemoveRetiredConfigKeys(path)
	if err != nil || len(removed) != 0 {
		t.Fatalf("second run = %v, %v, want nothing removed", removed, err)
	}
	if got, _ := os.ReadFile(backup); string(got) != retiredKeysConfig {
		t.Fatal("the second run touched the backup")
	}
}

func TestRemoveRetiredConfigKeysFirstAndLastMembers(t *testing.T) {
	for name, tc := range map[string][2]string{
		"first": {"{\"projects\": [], \"timezone\": \"UTC\", \"state_path\": \"/tmp/s.json\"}\n", "{\"timezone\": \"UTC\", \"state_path\": \"/tmp/s.json\"}\n"},
		"last":  {"{\"timezone\": \"UTC\", \"state_path\": \"/tmp/s.json\", \"projects\": []}\n", "{\"timezone\": \"UTC\", \"state_path\": \"/tmp/s.json\"}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc[0]), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := RemoveRetiredConfigKeys(path); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(path); string(got) != tc[1] {
				t.Fatalf("got %q, want %q", got, tc[1])
			}
		})
	}
}

func TestRemoveRetiredConfigKeysLeavesAnUnwritableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(retiredKeysConfig), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RemoveRetiredConfigKeys(path); err == nil {
		t.Fatal("a read-only config was rewritten")
	}
	if got, _ := os.ReadFile(path); string(got) != retiredKeysConfig {
		t.Fatal("a read-only config changed")
	}
	if _, err := os.Stat(path + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a backup was written for a config that is not rewritten")
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("the retired keys must still load (ignored): %v", err)
	}
}

func TestRemoveRetiredConfigKeysLeavesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(retiredKeysConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RemoveRetiredConfigKeys(link); err == nil {
		t.Fatal("a symlinked config was rewritten")
	}
	if got, _ := os.ReadFile(target); string(got) != retiredKeysConfig {
		t.Fatal("the symlink target changed")
	}
}

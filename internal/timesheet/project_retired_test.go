package timesheet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// copyFixtureStore copies the schema-3 fixture (captured from the previous
// build after a full stage, classify, save and submit) into a temporary store.
func copyFixtureStore(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "state_schema3_with_projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPriorSchemaStateUpgradesWithLegacyProjectsIntact(t *testing.T) {
	path := copyFixtureStore(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var prior State
	if err := json.Unmarshal(before, &prior); err != nil {
		t.Fatal(err)
	}
	if prior.Schema != 3 {
		t.Fatalf("fixture schema = %d, want 3", prior.Schema)
	}

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open schema-3 store: %v", err)
	}
	if store.DurableSchema() != 3 {
		t.Fatalf("durable schema = %d, want 3 until the first write", store.DurableSchema())
	}
	st := store.State()
	if st.Schema != 4 {
		t.Fatalf("running schema = %d, want 4", st.Schema)
	}
	if st.Migration == nil || len(st.Migration.SchemaUpgrades) != 1 {
		t.Fatalf("schema upgrade receipt = %+v, want one pending receipt", st.Migration)
	}
	pending := st.Migration.SchemaUpgrades[0]
	if pending.FromSchema != 3 || pending.ToSchema != 4 || pending.Days != 1 ||
		pending.Intervals != 3 || pending.LegacyProjectLabels != 3 || pending.AppliedAt != "" {
		t.Fatalf("pending receipt = %+v", pending)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("opening the store rewrote the schema-3 file")
	}
	if err := store.CommitState(st); err != nil {
		t.Fatalf("activate schema 4: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen schema-4 store: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.DurableSchema() != 4 {
		t.Fatalf("durable schema = %d, want 4", reopened.DurableSchema())
	}
	got := reopened.State()
	if got.Migration == nil || len(got.Migration.SchemaUpgrades) != 1 {
		t.Fatalf("receipts after activation = %+v, want exactly one", got.Migration)
	}
	if r := got.Migration.SchemaUpgrades[0]; r.FromSchema != 3 || r.ToSchema != 4 || r.AppliedAt == "" || r.AppVersion != AppVersion {
		t.Fatalf("durable receipt = %+v", r)
	}
	for date, day := range prior.Days {
		now := got.Days[date]
		if now == nil || now.Revision != day.Revision || len(now.Confirmed) != len(day.Confirmed) || len(now.History) != len(day.History) {
			t.Fatalf("day %s changed: %+v", date, now)
		}
		for i, iv := range day.Confirmed {
			if !intervalsEqual([]Interval{iv}, []Interval{now.Confirmed[i]}) || now.Confirmed[i].Project != "WORK" {
				t.Fatalf("interval %d changed: %+v, want %+v with its legacy project", i, now.Confirmed[i], iv)
			}
		}
	}
	if len(got.Remote.Bindings) != len(prior.Remote.Bindings) || len(got.Plans) != len(prior.Plans) ||
		len(got.Approvals) != len(prior.Approvals) || len(got.Intents) != len(prior.Intents) ||
		len(got.Executions) != len(prior.Executions) {
		t.Fatal("remote identities or submission history changed across the upgrade")
	}
	for i, b := range prior.Remote.Bindings {
		if got.Remote.Bindings[i].LocalID != b.LocalID || got.Remote.Bindings[i].RemoteID != b.RemoteID ||
			got.Remote.Bindings[i].ClassificationID != b.ClassificationID {
			t.Fatalf("binding %d changed: %+v", i, got.Remote.Bindings[i])
		}
	}
	if err := reopened.CommitState(reopened.State()); err != nil {
		t.Fatal(err)
	}
	if n := len(reopened.State().Migration.SchemaUpgrades); n != 1 {
		t.Fatalf("receipts after a second write = %d, want 1", n)
	}
}

func TestFreshStoreHasNoUpgradeReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.CommitState(store.State()); err != nil {
		t.Fatal(err)
	}
	if m := store.State().Migration; m != nil && len(m.SchemaUpgrades) != 0 {
		t.Fatalf("a new store recorded an upgrade: %+v", m.SchemaUpgrades)
	}
}

func TestSchemaThreeIntervalStillNeedsItsProject(t *testing.T) {
	path := copyFixtureStore(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	day := raw["days"].(map[string]any)["2026-10-26"].(map[string]any)
	delete(day["confirmed"].([]any)[0].(map[string]any), "project")
	data, _ = json.Marshal(raw)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path); errorCode(err) != CodeStateInvalid {
		t.Fatalf("schema-3 interval without a project = %v, want state_invalid in its own shape", err)
	}
}

func TestConfigNeedsNoProjectAndIgnoresRetiredKeys(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"none": `{"timezone": "Europe/Amsterdam", "state_path": "` + filepath.Join(dir, "s.json") + `"}`,
		"retired": `{"timezone": "Europe/Amsterdam", "state_path": "` + filepath.Join(dir, "s.json") + `",
  "projects": [{"id": "A", "default_classification": "DEV"}, {"id": "B"}],
  "migration": {"enabled": false, "project_map": {"OLD": "NOT-CONFIGURED"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err != nil {
				t.Fatalf("load: %v", err)
			}
		})
	}
	typo := filepath.Join(dir, "typo.json")
	if err := os.WriteFile(typo, []byte(`{"timezone": "Europe/Amsterdam", "state_path": "/tmp/s.json", "projcts": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(typo); err == nil {
		t.Fatal("an unknown key must still fail closed")
	}
}

func TestNewIntervalCarriesNoProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cfg := &Config{Timezone: "Europe/Amsterdam", StatePath: path}
	session := NewSession(cfg, map[string]string{}, store)
	requireOK(t, call(t, session, Request{ID: "1", Op: "open_day", Day: "2026-10-30"}))
	requireOK(t, call(t, session, Request{
		ID: "2", Op: "add_interval", Day: "2026-10-30", Revision: revision(0),
		Text: "2026-10-30 09:00-10:00 SYN-1 Build the bridge",
	}))
	requireOK(t, call(t, session, Request{ID: "3", Op: "confirm", Day: "2026-10-30", Revision: revision(0)}))
	requireOK(t, call(t, session, Request{ID: "4", Op: "save", Day: "2026-10-30", Revision: revision(1)}))
	iv := store.State().Days["2026-10-30"].Confirmed[0]
	if iv.Project != "" || iv.Classification != "" || iv.Description != "SYN-1 Build the bridge" {
		t.Fatalf("interval = %+v, want no project and the whole text as its description", iv)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Schema int `json:"schema"`
		Days   map[string]struct {
			Confirmed []map[string]any `json:"confirmed"`
		} `json:"days"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Schema != 4 {
		t.Fatalf("written schema = %d, want 4", raw.Schema)
	}
	if _, has := raw.Days["2026-10-30"].Confirmed[0]["project"]; has {
		t.Fatal("a new interval was written with a project key")
	}
}

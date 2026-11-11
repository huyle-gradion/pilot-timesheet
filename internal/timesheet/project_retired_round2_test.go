package timesheet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PR #292 round 2: the token editor's edge cases, backups that must never be
// trusted blindly, and a hard-linked configuration.

func TestWithoutRetiredKeysTokenEditor(t *testing.T) {
	cases := []struct {
		name, in, want string
		removed        int
	}{
		{"minified", `{"timezone":"UTC","projects":[{"id":"A"}],"state_path":"/s"}`, `{"timezone":"UTC","state_path":"/s"}`, 1},
		{"crlf", "{\r\n  \"projects\": [],\r\n  \"timezone\": \"UTC\"\r\n}\r\n", "{\r\n  \"timezone\": \"UTC\"\r\n}\r\n", 1},
		{"first", "{\n  \"projects\": [],\n  \"timezone\": \"UTC\"\n}\n", "{\n  \"timezone\": \"UTC\"\n}\n", 1},
		{"last", "{\n  \"timezone\": \"UTC\",\n  \"projects\": []\n}\n", "{\n  \"timezone\": \"UTC\"\n}\n", 1},
		{"only", "{\n  \"projects\": []\n}\n", "{}\n", 1},
		{"nested migration only member", "{\n  \"migration\": {\n    \"project_map\": {\"A\": \"B\"}\n  }\n}\n", "{\n  \"migration\": {}\n}\n", 1},
		{"nested migration middle", `{"migration": {"enabled": false, "project_map": {}, "classification_map": {}}}`, `{"migration": {"enabled": false, "classification_map": {}}}`, 1},
		{"duplicate keys", `{"projects": [], "projects": [1], "timezone": "UTC"}`, `{"timezone": "UTC"}`, 2},
		{"duplicate keys after a kept member", `{"timezone": "UTC", "projects": [], "projects": [1]}`, `{"timezone": "UTC"}`, 2},
		{"escaped key", `{"timezone": "UTC", "projects": []}`, `{"timezone": "UTC"}`, 1},
		{"case-insensitive key", `{"Projects": [], "timezone": "UTC", "Migration": {"Project_Map": {}}}`, `{"timezone": "UTC", "Migration": {}}`, 2},
		{"string value and nested key untouched", `{"timezone": "projects", "sources": {"projects": 1}}`, `{"timezone": "projects", "sources": {"projects": 1}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, removed, err := withoutRetiredKeys([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want || len(removed) != tc.removed {
				t.Fatalf("got %q (%v), want %q (%d removed)", got, removed, tc.want, tc.removed)
			}
		})
	}
}

func TestRemoveRetiredConfigKeysNeverReportsAForeignBackup(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, bak string){
		"unrelated file": func(t *testing.T, bak string) {
			if err := os.WriteFile(bak, []byte("unrelated"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"dangling symlink": func(t *testing.T, bak string) {
			if err := os.Symlink(filepath.Join(filepath.Dir(bak), "missing"), bak); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(retiredKeysConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			prepare(t, path+".bak")
			_, backup, err := RemoveRetiredConfigKeys(path)
			if err != nil {
				t.Fatal(err)
			}
			if backup != path+".bak.1" {
				t.Fatalf("backup = %q, want the next free name", backup)
			}
			if got, _ := os.ReadFile(backup); string(got) != retiredKeysConfig {
				t.Fatal("the reported backup does not hold the original bytes")
			}
		})
	}
	t.Run("identical existing backup is reused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(retiredKeysConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".bak", []byte(retiredKeysConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, backup, err := RemoveRetiredConfigKeys(path); err != nil || backup != path+".bak" {
			t.Fatalf("backup = %q, %v, want the identical existing .bak", backup, err)
		}
	})
	t.Run("no temporary file is left behind", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(retiredKeysConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := RemoveRetiredConfigKeys(path); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 2 {
			t.Fatalf("directory = %v, want only config.json and config.json.bak", entries)
		}
	})
}

func TestRemoveRetiredConfigKeysLeavesAHardLinkedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(retiredKeysConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "other.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RemoveRetiredConfigKeys(path); err == nil {
		t.Fatal("a hard-linked config was rewritten")
	}
	if got, _ := os.ReadFile(path); string(got) != retiredKeysConfig {
		t.Fatal("a hard-linked config changed")
	}
	if _, err := os.Stat(path + ".bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a backup was written for a config that is not rewritten")
	}
}

func TestSchemaUpgradeNeverTrustsAStaleOrDanglingBackup(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, bak string){
		"stale backup from an earlier rollback": func(t *testing.T, bak string) {
			if err := os.WriteFile(bak, []byte(`{"schema":3,"days":{}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"dangling symlink": func(t *testing.T, bak string) {
			if err := os.Symlink(filepath.Join(filepath.Dir(bak), "missing"), bak); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := copyFixtureStore(t)
			original, _ := os.ReadFile(path)
			prepare(t, path+".schema3.bak")
			notices := captureUpgradeNotices(t)
			store, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			if err := store.CommitState(store.State()); err != nil {
				t.Fatal(err)
			}
			want := path + ".schema3.bak.1"
			if got, _ := os.ReadFile(want); string(got) != string(original) {
				t.Fatalf("%s does not hold the schema-3 bytes", want)
			}
			if len(*notices) != 1 || !strings.Contains((*notices)[0], want) {
				t.Fatalf("notices = %q, want the line to name %s", *notices, want)
			}
		})
	}
}

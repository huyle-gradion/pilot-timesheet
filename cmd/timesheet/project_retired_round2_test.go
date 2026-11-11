package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRetiredConfigHookSaysIgnoredWhenNothingWasRemoved: when the load hook
// fires but the file is not rewritten (here a symlinked config), exactly one
// line says the keys are ignored, and the target is untouched.
func TestRetiredConfigHookSaysIgnoredWhenNothingWasRemoved(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	body := "{\"timezone\": \"UTC\", \"state_path\": \"/tmp/s.json\", \"projects\": []}\n"
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var lines []string
	hook := retiredConfigHook(func(line string) { lines = append(lines, line) })
	hook(link)
	hook(link)
	if len(lines) != 1 || !strings.Contains(lines[0], "ignored") {
		t.Fatalf("lines = %q, want one ignored line for a symlinked config", lines)
	}
	if got, _ := os.ReadFile(target); string(got) != body {
		t.Fatal("the symlink target changed")
	}
}

// TestRetiredConfigHookFlagsAKeyItCannotFind: a key the loader accepted but
// the editor did not remove still gets the ignored line, never silence.
func TestRetiredConfigHookFlagsAKeyItCannotFind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{\"timezone\": \"UTC\", \"state_path\": \"/tmp/s.json\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	retiredConfigHook(func(line string) { lines = append(lines, line) })(path)
	if len(lines) != 1 || !strings.Contains(lines[0], "ignored") {
		t.Fatalf("lines = %q, want one ignored line", lines)
	}
}

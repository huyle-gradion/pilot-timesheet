package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestShippedExampleConfigLoads(t *testing.T) {
	data, err := os.ReadFile("timesheet.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/Users/") {
		t.Fatal("example configuration uses a macOS home path; use a neutral placeholder")
	}
	if _, err := timesheet.LoadConfig("timesheet.example.json"); err != nil {
		t.Fatalf("timesheet.example.json does not load: %v", err)
	}
}

var jsonFence = regexp.MustCompile("(?s)```json\n(.*?)```")

func TestCollectionGuideExamplesLoad(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "agents", "collection-config.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := jsonFence.FindAllSubmatch(doc, -1)
	if len(blocks) == 0 {
		t.Fatal("collection guide has no JSON configuration example")
	}
	for i, b := range blocks {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, b[1], 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := timesheet.LoadConfig(path); err != nil {
			t.Errorf("collection guide example %d does not load: %v", i+1, err)
		}
	}
}

package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const writeBodiesGolden = "testdata/write_bodies_issue185.golden"

func TestRemoteWriteBodiesUnchangedForTheSameDay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stub := &stubSubmit{entries: map[string][]map[string]any{testDate: nil}, writeTools: true}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeTestConfigWithGradion(t, server.URL)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "08:00", "13:00", "Implement synthetic design"),
		timedBlock("work-2", "14:00", "15:00", "Review the deck"),
	})
	seedCatalog(t, cfg, "cat-1",
		timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"},
	)
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(stage.Staged) != 3 {
		t.Fatalf("stage = %+v, want 3 staged intervals (one block chunked in two)", stage.Staged)
	}
	selections := make([]string, 0, len(stage.Staged))
	for _, ref := range stage.Staged {
		selections = append(selections, `"`+ref.IntervalID+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"}`)
	}
	choices := writeChoicesFile(t, cfg, `{"selections": {`+strings.Join(selections, ",")+`}}`)
	if receipt := runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choices}); receipt.Selections != 3 {
		t.Fatalf("classify = %+v, want 3 selections", receipt)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	review := agentReview(t, cfg)
	if review.Plan.State != "ready" || len(review.Plan.Operations) != 3 {
		t.Fatalf("review = %+v, want a ready three-create plan", review)
	}
	captureStdout(t, func() {
		if err := runAgentSubmit(agentConfirmArgs(cfg, review)); err != nil {
			t.Fatal(err)
		}
	})
	stub.mu.Lock()
	got := strings.Join(stub.writeParams, "\n") + "\n"
	stub.mu.Unlock()
	if os.Getenv("UPDATE_WRITE_BODIES_GOLDEN") == "1" {
		if err := os.WriteFile(filepath.FromSlash(writeBodiesGolden), []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(filepath.FromSlash(writeBodiesGolden))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("remote write bodies changed:\n got: %s\nwant: %s", got, want)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		var params struct {
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(line), &params); err != nil {
			t.Fatal(err)
		}
		if _, has := params.Arguments["project"]; has {
			t.Fatalf("a write body carries a project: %s", line)
		}
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestGithubMergeRunCLI(t *testing.T) {
	if os.Getenv("TIMESHEET_MERGE_CLI_CHILD") == "1" {
		http.DefaultTransport = &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(address)
			if err != nil || !net.ParseIP(host).IsLoopback() {
				return nil, fmt.Errorf("test blocked non-loopback request")
			}
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, address)
		}}
		separator := slices.Index(os.Args, "--")
		if separator < 0 {
			t.Fatal("missing CLI arguments")
		}
		os.Args = append([]string{"timesheet"}, os.Args[separator+1:]...)
		main()
		os.Exit(0)
	}

	var searches, details atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Errorf("unexpected request method or authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		body := `[]`
		switch r.URL.Path {
		case "/search/issues":
			searches.Add(1)
			body = `{"total_count":0,"incomplete_results":false,"items":[]}`
			if strings.Contains(r.URL.Query().Get("q"), "merged:") {
				body = `{"total_count":1,"incomplete_results":false,"items":[{"number":12,"title":"search candidate","html_url":"https://example.invalid/o/r/pull/12","pull_request":{"url":"https://untrusted.invalid/pulls/999","merged_at":"2026-10-21T10:00:00Z"}}]}`
			}
		case "/repos/o/r/pulls/12":
			details.Add(1)
			body = `{"number":12,"title":"verified synthetic merge","html_url":"https://example.invalid/o/r/pull/12","merged_by":{"login":"user"},"merged_at":"2026-10-22T10:00:00Z"}`
		case "/repos/o/r/commits", "/repos/o/r/pulls/comments", "/repos/o/r/issues/comments", "/repos/o/r/events":
		default:
			t.Errorf("unexpected provider path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write provider response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	home := t.TempDir()
	statePath := filepath.Join(home, "state.json")
	configPath := filepath.Join(home, "config.json")
	envPath := filepath.Join(home, "synthetic.env")
	cfg := timesheet.Config{
		Timezone: "UTC", StatePath: statePath,
		Sources: &timesheet.SourcesConfig{Enabled: true, Github: &timesheet.GitHubSourceConfig{
			Enabled: true, User: "user", Repositories: []string{"o/r"}, APIBase: server.URL,
		}},
	}
	config, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("GITHUB_TOKEN=synthetic-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestGithubMergeRunCLI$", "--", "run",
		"--config", configPath, "--env-file", envPath, "--date", "2026-10-22")
	cmd.Dir = home
	cmd.Env = []string{"TIMESHEET_MERGE_CLI_CHILD=1", "HOME=" + home, "TMPDIR=" + home,
		"XDG_CONFIG_HOME=" + home, "GITHUB_TOKEN=synthetic-token"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("CLI run: %v; stderr=%s", err, stderr.String())
	}

	if searches.Load() != 3 || details.Load() != 1 {
		t.Fatalf("searches=%d details=%d, want three candidate searches and one detail verification", searches.Load(), details.Load())
	}
	var draft timesheet.Draft
	if err := json.Unmarshal(stdout.Bytes(), &draft); err != nil {
		t.Fatalf("decode CLI draft: %v", err)
	}
	if draft.Date != "2026-10-22" || len(draft.Blocks) != 1 || !slices.Contains(draft.Blocks[0].PRs, "#12 o/r") {
		t.Fatalf("CLI draft lost verified merge activity: %+v", draft)
	}
	store, err := timesheet.OpenStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	evidence := store.Evidence("2026-10-22")
	if evidence == nil || len(evidence.Attempts) != 1 || evidence.Attempts[0].Source != timesheet.SourceGithub ||
		evidence.Attempts[0].Status != timesheet.StatusComplete || evidence.Attempts[0].Acknowledged {
		t.Fatalf("source attempt must be complete without acknowledgment: %+v", evidence)
	}
	if len(evidence.Topics) != 1 || len(evidence.Topics[0].Items) != 1 {
		t.Fatalf("persisted merge evidence: %+v", evidence.Topics)
	}
	item := evidence.Topics[0].Items[0]
	if item.Ref != "gh:o/r:pr-merge:12" || item.Kind != "merged_pull_request" || item.Resource != "pr:o/r:12" ||
		item.Actor != "user" || item.Time != "2026-10-22T10:00:00Z" || item.Title != "verified synthetic merge" ||
		item.URL != "https://example.invalid/o/r/pull/12" || item.Provenance != "search/issues,repos/pulls/12" {
		t.Fatalf("persisted item must use authoritative PR details: %+v", item)
	}
}

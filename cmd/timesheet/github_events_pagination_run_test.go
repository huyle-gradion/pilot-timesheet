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
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestGithubEventsPaginationRunCLI(t *testing.T) {
	if os.Getenv("TIMESHEET_EVENTS_PAGINATION_CHILD") == "1" {
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
	for _, terminalLink := range []bool{true, false} {
		t.Run(fmt.Sprintf("terminal_link_%t", terminalLink), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Error("missing original-origin authentication")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/search/issues":
					if _, err := fmt.Fprint(w, `{"total_count":0,"incomplete_results":false,"items":[]}`); err != nil {
						t.Error(err)
					}
				case "/repos/o/r/events":
					requests.Add(1)
					page, err := strconv.Atoi(r.URL.Query().Get("page"))
					if err != nil {
						t.Error(err)
					}
					if page > 3 {
						w.WriteHeader(http.StatusUnprocessableEntity)
						return
					}
					if page == 3 && terminalLink {
						w.Header().Set("Link", `<https://api.github.com/repos/o/r/events?page=2>; rel="prev", <https://api.github.com/repos/o/r/events?page=1>; rel="first"`)
					} else {
						w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com/repos/o/r/events?page=%d>; rel="next"`, page+1))
					}
					docs := make([]struct {
						ID        string `json:"id"`
						Type      string `json:"type"`
						CreatedAt string `json:"created_at"`
						Actor     struct {
							Login string `json:"login"`
						} `json:"actor"`
					}, 100)
					for i := range docs {
						docs[i].ID = strconv.Itoa(page*100 + i)
						docs[i].Type = "WatchEvent"
						docs[i].CreatedAt = "2026-10-22T12:00:00Z"
						docs[i].Actor.Login = "other"
					}
					docs[0].Actor.Login = "user"
					if err := json.NewEncoder(w).Encode(docs); err != nil {
						t.Error(err)
					}
				case "/repos/o/r/commits", "/repos/o/r/pulls/comments", "/repos/o/r/issues/comments":
					if _, err := fmt.Fprint(w, `[]`); err != nil {
						t.Error(err)
					}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
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
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestGithubEventsPaginationRunCLI$", "--", "run",
				"--config", configPath, "--env-file", envPath, "--date", "2026-10-22")
			cmd.Dir = home
			cmd.Env = []string{"TIMESHEET_EVENTS_PAGINATION_CHILD=1", "HOME=" + home, "TMPDIR=" + home,
				"XDG_CONFIG_HOME=" + home, "GITHUB_TOKEN=synthetic-token"}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr

			runErr := cmd.Run()

			wantRequests, wantStatus, wantFamily := int32(4), timesheet.StatusIncomplete, timesheet.StatusIncomplete
			if terminalLink {
				wantRequests, wantStatus, wantFamily = 3, timesheet.StatusComplete, timesheet.StatusWindowed
			}
			if runErr != nil || requests.Load() != wantRequests {
				t.Fatalf("requests=%d error=%v stdout=%s stderr=%s", requests.Load(), runErr, stdout.String(), stderr.String())
			}
			var draft timesheet.Draft
			if err := json.Unmarshal(stdout.Bytes(), &draft); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(draft.Flags, "source_incomplete:github") == terminalLink {
				t.Fatalf("unexpected source coverage flags %v", draft.Flags)
			}
			store, err := timesheet.OpenStore(statePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()
			evidence := store.Evidence("2026-10-22")
			if evidence == nil || len(evidence.Attempts) != 1 || evidence.Attempts[0].Status != wantStatus || evidence.Attempts[0].Acknowledged {
				t.Fatalf("unexpected source attempt %+v", evidence)
			}
			for _, family := range evidence.Attempts[0].Families {
				if family.Family == "repo_events" {
					if family.Status != wantFamily || family.Count != 3 {
						t.Fatalf("unexpected events family %+v", family)
					}
					return
				}
			}
			t.Fatal("missing persisted repo_events family")
		})
	}
}

package timesheet

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func TestTerminalLinkCoveragePreservesSourceGate(t *testing.T) {
	for _, terminalLink := range []bool{true, false} {
		name := "genuine 422 requires acknowledgment"
		if terminalLink {
			name = "terminal Link needs no acknowledgment"
		}
		t.Run(name, func(t *testing.T) {
			requests := 0
			fixture := newGithubStatusFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests > 3 {
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				if requests == 3 && terminalLink {
					w.Header().Set("Link", `<https://example.invalid/?page=2>; rel="prev", <https://example.invalid/?page=1>; rel="first"`)
				} else {
					w.Header().Set("Link", `<https://example.invalid/>; rel="next"`)
				}
				if err := json.NewEncoder(w).Encode(syntheticEventPage(100)); err != nil {
					t.Error(err)
				}
			}, nil)
			cfg := &Config{
				Timezone: "UTC", StatePath: filepath.Join(t.TempDir(), "state.json"),
				Sources: &SourcesConfig{Enabled: true, Github: &GitHubSourceConfig{
					Enabled: true, User: statusTestUser, Repositories: []string{statusTestRepo}, APIBase: fixture.server.URL,
				}},
			}
			store, err := OpenStore(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()
			session := NewSession(cfg, map[string]string{"GITHUB_TOKEN": "synthetic-token"}, store)
			day := "2026-10-22"
			if _, err := session.openDay(Request{ID: "1", Op: "open_day", Day: day}); err != nil {
				t.Fatal(err)
			}
			if _, err := session.collectDay(Request{ID: "2", Op: "collect", Day: day}); err != nil {
				t.Fatal(err)
			}

			gateErr := session.checkSourceGate(day)

			wantRequests := 4
			if terminalLink {
				wantRequests = 3
			}
			if (gateErr == nil) != terminalLink || requests != wantRequests {
				t.Fatalf("requests=%d gate error=%v; want %d requests and allowed=%t", requests, gateErr, wantRequests, terminalLink)
			}
			if evidence := store.Evidence(day); evidence.Attempts[0].Acknowledged {
				t.Fatal("collection must not synthesize an acknowledgment")
			}
		})
	}
}

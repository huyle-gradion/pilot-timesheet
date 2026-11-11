package timesheet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestGithubFamiliesAttributeOnlyProviderNamedConfiguredUser(t *testing.T) {
	at := "2026-10-22T10:00:00Z"
	commit := func(sha string, author any) map[string]any {
		return map[string]any{"sha": sha, "author": author,
			"commit": map[string]any{"message": sha, "author": map[string]any{"email": "me@example.invalid", "date": at}}}
	}
	serve := func(mux *http.ServeMux, path string, body any) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(body) })
	}
	mux := http.NewServeMux()
	serve(mux, "/repos/o/r/commits", []any{
		commit("mine", map[string]any{"login": "me"}), commit("theirs", map[string]any{"login": "other"}), commit("unverified", nil),
	})
	serve(mux, "/search/issues", map[string]any{"total_count": 2, "items": []any{
		map[string]any{"number": 7, "created_at": at, "user": map[string]any{"login": "me"}},
		map[string]any{"number": 8, "created_at": at, "user": map[string]any{"login": "other"}},
	}})
	serve(mux, "/repos/o/r/pulls/7/reviews", []any{
		map[string]any{"id": 1, "state": "APPROVED", "submitted_at": at, "user": map[string]any{"login": "me"}},
		map[string]any{"id": 2, "state": "APPROVED", "submitted_at": at, "user": map[string]any{"login": "other"}},
	})
	mux.HandleFunc("/repos/o/r/pulls/8/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("[]")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	run := &githubRun{scope: &githubScope{user: "me", repos: []string{"o/r"}, apiBase: base, token: "synthetic"}, client: srv.Client()}
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	commits, rep := fetchCommits(run, "o/r", "o", "r", start, end)
	if len(commits) != 1 || commits[0].Ref != "gh:commit:mine" || rep.Status != StatusIncomplete {
		t.Fatalf("commits = %+v, report %+v; want only the user's commit and an incomplete family", commits, rep)
	}
	if pulls, _ := fetchAuthoredPulls(run, "o/r", "o", "r", start, end); len(pulls) != 1 || pulls[0].Ref != "gh:o/r:pr:7" {
		t.Fatalf("authored pulls = %+v, want only #7", pulls)
	}
	if reviews, _ := fetchSubmittedReviews(run, "o/r", "o", "r", start, end); len(reviews) != 1 || reviews[0].Ref != "gh:o/r:review:1" {
		t.Fatalf("reviews = %+v, want only review 1", reviews)
	}
	if key, _ := githubTopicKey(commits[0]); key != "gh:o/r:commits" {
		t.Fatalf("commit topic key = %q, want the per-repository commit group", key)
	}
}

func TestAuthoredPullSearchBoundsMakeFamilyIncomplete(t *testing.T) {
	for name, doc := range map[string]map[string]any{
		"incomplete_results": {"total_count": 1, "incomplete_results": true, "items": []any{}},
		"over 1000 results":  {"total_count": 1001, "incomplete_results": false, "items": []any{}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(doc) }))
		base, _ := url.Parse(srv.URL)
		run := &githubRun{scope: &githubScope{user: "me", apiBase: base, token: "synthetic"}, client: srv.Client()}
		start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
		_, rep := fetchAuthoredPulls(run, "o/r", "o", "r", start, start.Add(24*time.Hour))
		srv.Close()
		if rep.Status != StatusIncomplete {
			t.Fatalf("%s: authored pulls report = %+v, want incomplete", name, rep)
		}
	}
}

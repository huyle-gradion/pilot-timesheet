package timesheet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	statusTestUser = "user"
	statusTestRepo = "o/r"
)

var statusTestWindow = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)

func syntheticGithubEvent(id int, login, at string) map[string]any {
	return map[string]any{
		"id":         "evt-" + strconv.Itoa(id),
		"type":       "WatchEvent",
		"actor":      map[string]any{"login": login},
		"created_at": at,
	}
}

func syntheticEventPage(n int) []map[string]any {
	items := make([]map[string]any, n)
	for i := range items {
		login := "other-actor"
		if i%7 == 0 {
			login = statusTestUser
		}
		items[i] = syntheticGithubEvent(n*1000+i, login, "2026-10-22T10:00:00Z")
	}
	return items
}

func syntheticAttributablePage(n int) []map[string]any {
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = syntheticGithubEvent(n*1000+i, statusTestUser, "2026-10-22T10:00:00Z")
	}
	return items
}

type githubStatusFixture struct {
	mux                  *http.ServeMux
	server               *httptest.Server
	searchCalls          *int
	issueCommentsHandler http.HandlerFunc
	issueLookupHandler   http.HandlerFunc
	issueLookupCalls     *int
}

func newGithubStatusFixture(t *testing.T, eventsHandler, searchHandler http.HandlerFunc) *githubStatusFixture {
	t.Helper()
	f := &githubStatusFixture{mux: http.NewServeMux()}
	jsonOK := func(w http.ResponseWriter) { w.Header().Set("Content-Type", "application/json") }

	f.mux.HandleFunc("/repos/o/r/commits", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	f.mux.HandleFunc("/repos/o/r/pulls/comments", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	f.mux.HandleFunc("/repos/o/r/issues/comments", func(w http.ResponseWriter, r *http.Request) {
		if f.issueCommentsHandler != nil {
			f.issueCommentsHandler(w, r)
			return
		}
		jsonOK(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	f.issueLookupCalls = new(int)
	f.mux.HandleFunc("/repos/o/r/issues/", func(w http.ResponseWriter, r *http.Request) {
		*f.issueLookupCalls++
		if f.issueLookupHandler != nil {
			f.issueLookupHandler(w, r)
			return
		}
		http.NotFound(w, r)
	})
	f.mux.HandleFunc("/repos/o/r/pulls/0/reviews", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	f.searchCalls = new(int)
	f.mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		*f.searchCalls++
		if searchHandler != nil {
			searchHandler(w, r)
			return
		}
		jsonOK(w)
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "incomplete_results": false, "items": []map[string]any{}})
	})
	f.mux.HandleFunc("/repos/o/r/events", func(w http.ResponseWriter, r *http.Request) {
		if eventsHandler != nil {
			eventsHandler(w, r)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		jsonOK(w)
		if page == 1 {
			_ = json.NewEncoder(w).Encode(syntheticEventPage(100))
			return
		}
		_ = json.NewEncoder(w).Encode(syntheticEventPage(37))
	})
	f.server = httptest.NewServer(f.mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *githubStatusFixture) run(t *testing.T) *githubRun {
	base, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatalf("parse synthetic base URL: %v", err)
	}
	return &githubRun{
		scope:  &githubScope{user: statusTestUser, repos: []string{statusTestRepo}, apiBase: base, token: "synthetic-token"},
		client: f.server.Client(),
	}
}

func TestRepoEvents422PastRetainedWindowIsIncomplete(t *testing.T) {
	requests := 0
	f := newGithubStatusFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		if page > 3 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"We were unable to complete your request.","documentation_url":"https://docs.github.com/rest/activity/events#list-repository-events"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(syntheticAttributablePage(100))
	}, nil)

	items, report := f.fetchRepoEventsWindow(t)

	if requests != 4 {
		t.Fatalf("events requests = %d, want 4 (three full pages, then the 422)", requests)
	}
	if report.Status != StatusIncomplete {
		t.Fatalf("family status = %q, want %q (bounded provider coverage, never failed)", report.Status, StatusIncomplete)
	}
	if report.Count != 300 || len(items) != 300 {
		t.Fatalf("retained count = %d (report %d), want 300", len(items), report.Count)
	}
	if !strings.Contains(report.Detail, "HTTP 422") {
		t.Fatalf("detail %q must name the HTTP status", report.Detail)
	}
	if !strings.Contains(report.Detail, "unable to complete") {
		t.Fatalf("detail %q must carry a bounded body snippet", report.Detail)
	}
	if !strings.Contains(report.Detail, "300") {
		t.Fatalf("detail %q must record the retained count", report.Detail)
	}
	if strings.Contains(report.Detail, "https://") || strings.Contains(report.Detail, "docs.github.com") {
		t.Fatalf("detail %q must not echo URLs", report.Detail)
	}
	if strings.Contains(report.Detail, "synthetic-token") {
		t.Fatalf("detail %q must never carry the credential", report.Detail)
	}
}

func TestRepoEvents422AggregateCountsOneLimitedFamily(t *testing.T) {
	f := newGithubStatusFixture(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		if page > 3 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"We were unable to complete your request.","documentation_url":"https://docs.github.com/x"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(syntheticAttributablePage(100))
	}, nil)

	got := collectGithub(&githubScope{
		user:    statusTestUser,
		repos:   []string{statusTestRepo},
		apiBase: mustParseURL(t, f.server.URL),
		token:   "synthetic-token",
	}, statusTestWindow, statusTestWindow.Add(24*time.Hour))

	if got.Status != StatusIncomplete {
		t.Fatalf("source status = %q, want %q", got.Status, StatusIncomplete)
	}
	if !strings.Contains(got.Detail, "1 of 7 family queries reported limited or failed coverage") {
		t.Fatalf("aggregate detail = %q, want the 1-of-6 bounded-family wording", got.Detail)
	}
}

func TestRepoEventsCleanWalkIsWindowedAndAggregateComplete(t *testing.T) {
	f := newGithubStatusFixture(t, nil, nil)

	items, report := f.fetchRepoEventsWindow(t)
	if report.Status != StatusWindowed {
		t.Fatalf("family status = %q, want %q", report.Status, StatusWindowed)
	}
	if report.Count != 21 || len(items) != 21 {
		t.Fatalf("retained count = %d (report %d), want 21", len(items), report.Count)
	}
	if !strings.Contains(report.Detail, "windowed coverage") {
		t.Fatalf("detail %q must describe the bounded window honestly", report.Detail)
	}

	got := collectGithub(&githubScope{
		user:    statusTestUser,
		repos:   []string{statusTestRepo},
		apiBase: mustParseURL(t, f.server.URL),
		token:   "synthetic-token",
	}, statusTestWindow, statusTestWindow.Add(24*time.Hour))
	if got.Status != StatusComplete {
		t.Fatalf("source status = %q (%s), want %q for a fully collected day", got.Status, got.Detail, StatusComplete)
	}
	if strings.Contains(got.Detail, "limited or failed coverage") {
		t.Fatalf("aggregate detail %q must not count windowed coverage as limited", got.Detail)
	}
	var eventsReport *FamilyReport
	for i := range got.Families {
		if got.Families[i].Family == "repo_events" {
			eventsReport = &got.Families[i]
		}
	}
	if eventsReport == nil || eventsReport.Status != StatusWindowed {
		t.Fatalf("repo_events family report missing or not windowed: %+v", eventsReport)
	}
}

func TestRepoEventsQuietFeedIsWindowed(t *testing.T) {
	f := newGithubStatusFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}, nil)

	_, report := f.fetchRepoEventsWindow(t)
	if report.Status != StatusWindowed {
		t.Fatalf("family status = %q, want %q (not empty)", report.Status, StatusWindowed)
	}
	if report.Count != 0 {
		t.Fatalf("count = %d, want 0", report.Count)
	}
}

func TestRepoEvents403StaysFailed(t *testing.T) {
	f := newGithubStatusFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic permission failure", http.StatusForbidden)
	}, nil)

	_, report := f.fetchRepoEventsWindow(t)
	if report.Status != StatusFailed {
		t.Fatalf("family status = %q, want %q unchanged", report.Status, StatusFailed)
	}
	if !strings.Contains(report.Detail, "HTTP 403") {
		t.Fatalf("detail %q must name the provider failure", report.Detail)
	}
}

func TestSearchFamiliesBoundToMaxSearchResultPages(t *testing.T) {
	alwaysFullPage := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		items := make([]map[string]any, 100)
		for i := range items {
			items[i] = map[string]any{"number": 0, "title": "synthetic"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1000, "incomplete_results": false, "items": items})
	}

	t.Run("authored_pull_requests", func(t *testing.T) {
		f := newGithubStatusFixture(t, nil, alwaysFullPage)
		_, report := fetchAuthoredPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, statusTestWindow.Add(24*time.Hour))
		if *f.searchCalls != maxSearchResultPages {
			t.Fatalf("search requests = %d, want %d (maxSearchResultPages must bind)", *f.searchCalls, maxSearchResultPages)
		}
		if report.Status != StatusIncomplete {
			t.Fatalf("family status = %q, want %q", report.Status, StatusIncomplete)
		}
		if !strings.Contains(report.Detail, fmt.Sprintf("%d-page pagination ceiling", maxSearchResultPages)) {
			t.Fatalf("detail %q must name the maxSearchResultPages ceiling", report.Detail)
		}
	})

	t.Run("merged_pull_requests", func(t *testing.T) {
		f := newGithubStatusFixture(t, nil, alwaysFullPage)
		_, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, statusTestWindow.Add(24*time.Hour))
		if *f.searchCalls != maxSearchResultPages {
			t.Fatalf("search requests = %d, want %d (maxSearchResultPages must bind)", *f.searchCalls, maxSearchResultPages)
		}
		if report.Status != StatusIncomplete {
			t.Fatalf("family status = %q, want %q", report.Status, StatusIncomplete)
		}
		if !strings.Contains(report.Detail, fmt.Sprintf("%d-page pagination ceiling", maxSearchResultPages)) {
			t.Fatalf("detail %q must name the maxSearchResultPages ceiling", report.Detail)
		}
	})

	t.Run("submitted_review_candidate_search", func(t *testing.T) {
		f := newGithubStatusFixture(t, nil, alwaysFullPage)
		_, report := fetchSubmittedReviews(f.run(t), statusTestRepo, "o", "r", statusTestWindow, statusTestWindow.Add(24*time.Hour))
		if *f.searchCalls != maxSearchResultPages {
			t.Fatalf("search requests = %d, want %d (maxSearchResultPages must bind)", *f.searchCalls, maxSearchResultPages)
		}
		if report.Status != StatusIncomplete {
			t.Fatalf("family status = %q, want %q", report.Status, StatusIncomplete)
		}
		if !strings.Contains(report.Detail, fmt.Sprintf("%d-page pagination ceiling", maxSearchResultPages)) {
			t.Fatalf("detail %q must name the maxSearchResultPages ceiling", report.Detail)
		}
	})
}

func TestWindowedDayPassesSourceGateWithoutAcknowledge(t *testing.T) {
	f := newGithubStatusFixture(t, nil, nil)
	cfg := &Config{
		Timezone:  "UTC",
		StatePath: filepath.Join(t.TempDir(), "state.json"),
		Sources: &SourcesConfig{
			Enabled: true,
			Github: &GitHubSourceConfig{
				Enabled:      true,
				User:         statusTestUser,
				Repositories: []string{statusTestRepo},
				APIBase:      f.server.URL,
			},
		},
	}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := NewSession(cfg, map[string]string{"GITHUB_TOKEN": "synthetic-token"}, store)
	day := "2026-10-22"
	if _, err := s.openDay(Request{ID: "1", Op: "open_day", Day: day}); err != nil {
		t.Fatal(err)
	}
	col, err := s.collectDay(Request{ID: "2", Op: "collect", Day: day})
	if err != nil {
		t.Fatal(err)
	}
	if len(col.Sources) != 1 || col.Sources[0].Source != SourceGithub {
		t.Fatalf("collect summary: %+v", col.Sources)
	}
	if col.Sources[0].Status != StatusComplete {
		t.Fatalf("source status = %q (%s), want %q", col.Sources[0].Status, col.Sources[0].Detail, StatusComplete)
	}
	if col.Sources[0].Acknowledged {
		t.Fatal("nothing was acknowledged; the summary must not claim it")
	}

	ev := store.Evidence(day)
	if ev == nil || len(ev.Attempts) != 1 {
		t.Fatalf("persisted evidence: %+v", ev)
	}
	var windowed bool
	for _, fam := range ev.Attempts[0].Families {
		if fam.Family == "repo_events" {
			windowed = fam.Status == StatusWindowed
		}
	}
	if !windowed {
		t.Fatalf("persisted repo_events family must be windowed: %+v", ev.Attempts[0].Families)
	}
	if err := validateEvidence(ev); err != nil {
		t.Fatalf("persisted evidence with a windowed family must validate: %v", err)
	}
	if validEvidenceStatus(StatusWindowed) || !validEvidenceStatus(StatusIncomplete) || validEvidenceStatus("bogus") {
		t.Fatal("validEvidenceStatus must keep its original attempt-level enum (windowed is family-only)")
	}
	if statusesAckable(StatusComplete) {
		t.Fatal("a complete source must not be ackable: the gate passes without acknowledgment")
	}
	if err := s.checkSourceGate(day); err != nil {
		t.Fatalf("a windowed-only day must pass the source gate without acknowledge: %v", err)
	}
}

func (f *githubStatusFixture) fetchRepoEventsWindow(t *testing.T) ([]EvidenceItem, FamilyReport) {
	t.Helper()
	return fetchRepoEvents(f.run(t), statusTestRepo, "o", "r", statusTestWindow, statusTestWindow.Add(24*time.Hour))
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u
}

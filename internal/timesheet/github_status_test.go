package timesheet

// github_status_test.go locks the repo-events honesty semantics of the GitHub
// collector (issue #149) against synthetic loopback providers only — no live
// GitHub access, no network outside the process:
//
//   - a 422 on the events page past the provider's retained window is family
//     `incomplete` (bounded provider coverage, retained count kept, sanitized
//     status+body diagnostic), never `failed`;
//   - a fully-walked repo_events family (busy or quiet) reports the dedicated
//     `windowed` status, never `empty`/`complete`/`incomplete`;
//   - the aggregate source detail counts only `failed` + `incomplete`
//     families, so a fully collected day reads complete, not "1 of 7";
//   - a genuine 403 still reports `failed`;
//   - the three search families run under maxSearchResultPages, not the shared
//     REST ceiling;
//   - a windowed-only day passes the source gate without any acknowledgment.

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

// syntheticGithubEvent builds one attributable events-timeline document
// (WatchEvent falls through to the collector's default branch, which retains
// any attributable event with a provider id).
func syntheticGithubEvent(id int, login, at string) map[string]any {
	return map[string]any{
		"id":         "evt-" + strconv.Itoa(id),
		"type":       "WatchEvent",
		"actor":      map[string]any{"login": login},
		"created_at": at,
	}
}

// syntheticEventPage returns n events for one page. Only every 7th event is
// attributable to the configured user, so a clean walk retains few items and
// stays clear of the 50-item topic cap — these tests pin status semantics,
// not topic volume. Pagination still counts all n documents per page.
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

// syntheticAttributablePage returns n events all attributable to the
// configured user, so a retained count can be pinned exactly (the 422 walk).
func syntheticAttributablePage(n int) []map[string]any {
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = syntheticGithubEvent(n*1000+i, statusTestUser, "2026-10-22T10:00:00Z")
	}
	return items
}

// githubStatusFixture is a loopback provider serving clean defaults for all
// seven collector families. eventsHandler, when non-nil, replaces the default
// repo-events handler; searchHandler likewise replaces the clean
// /search/issues default. issueCommentsHandler and issueLookupHandler replace
// the issue-comments and issue-document handlers when set before the first
// request (issue #175 classification tests). Counters record how many requests
// each saw.
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
		// Default clean walk: a full page then a partial page (finished).
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

// TestRepoEvents422PastRetainedWindowIsIncomplete locks the #149 pin: pages
// 1..3 return exactly 100 items, page 4 returns GitHub's documented 422 for
// the page past the retained 300-event window; the family must report
// `incomplete` with the retained count and a sanitized diagnostic (HTTP status
// plus bounded body snippet, no URLs), never `failed`.
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

// TestRepoEvents422AggregateCountsOneLimitedFamily pins the aggregate wording:
// a 422-limited repo_events family among otherwise clean families reads
// "1 of 7 family queries reported limited or failed coverage" and the source
// stays incomplete.
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

// TestRepoEventsCleanWalkIsWindowedAndAggregateComplete locks the steady
// state: a fully-walked busy events feed reports the documented `windowed`
// status (bounded by the provider's design, not empty/complete/incomplete),
// and the aggregate over otherwise-complete families reads source status
// `complete` with no "limited or failed coverage" wording.
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

// TestRepoEventsQuietFeedIsWindowed pins that a fully-walked quiet feed is
// `windowed`, not `empty`: the window is bounded by the provider's design
// regardless of how few items it retained.
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

// TestRepoEvents403StaysFailed pins that a genuine permission failure keeps
// the `failed` classification unchanged.
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

// TestSearchFamiliesBoundToMaxSearchResultPages pins that the dead constant
// now binds: each search family stops after maxSearchResultPages pages with
// more pages available, and reports the 10-page ceiling in its detail.
func TestSearchFamiliesBoundToMaxSearchResultPages(t *testing.T) {
	alwaysFullPage := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		items := make([]map[string]any, 100)
		for i := range items {
			// number:0 candidates are never expanded into review verification
			// calls, so one handler serves both search families.
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

// TestWindowedDayPassesSourceGateWithoutAcknowledge pins the gate contract:
// a day whose only coverage bound is the provider-designed events window has
// source status `complete`, passes checkSourceGate with no acknowledgment,
// and its persisted evidence (which carries the `windowed` family status)
// passes the store validator.
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

	// The persisted attempt carries the windowed repo_events family and must
	// stay structurally valid, and the gate must pass without acknowledgment.
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
	// validEvidenceStatus enforces only the attempt-level status enum, which
	// never carries "windowed"; family statuses are report-only and are not
	// load-validated.
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

// fetchRepoEventsWindow runs the events family against the fixture's provider
// for the synthetic day window.
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

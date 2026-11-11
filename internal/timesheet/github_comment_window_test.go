package timesheet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func commentWindowRun(t *testing.T, handler http.HandlerFunc) (*githubRun, *int) {
	t.Helper()
	calls := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse synthetic base URL: %v", err)
	}
	return &githubRun{
		scope:  &githubScope{user: statusTestUser, repos: []string{statusTestRepo}, apiBase: base, token: "synthetic-token"},
		client: srv.Client(),
	}, calls
}

func commentWindowReviewDoc(id int, login, at string) map[string]any {
	return map[string]any{
		"id":               id,
		"created_at":       at,
		"pull_request_url": fmt.Sprintf("https://api.github.invalid/repos/o/r/pulls/%d", id),
		"html_url":         fmt.Sprintf("https://example.invalid/o/r/pull/%d#discussion_r%d", id, id),
		"user":             map[string]any{"login": login},
	}
}

func commentWindowIssueDoc(id, issue int, login, at string) map[string]any {
	return map[string]any{
		"id":         id,
		"created_at": at,
		"issue_url":  fmt.Sprintf("https://api.github.invalid/repos/o/r/issues/%d", issue),
		"html_url":   fmt.Sprintf("https://example.invalid/o/r/issues/%d#issuecomment-%d", issue, id),
		"user":       map[string]any{"login": login},
	}
}

func TestReviewCommentQueryReachesTheDayWithSinceOverlap(t *testing.T) {
	start, end := statusTestWindow, powWindowEnd()
	wantSince := start.Add(-time.Minute).UTC().Format(time.RFC3339)
	run, calls := commentWindowRun(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("since") != wantSince {
			t.Errorf("since = %q, want %q (the strict-after bound needs the overlap)", q.Get("since"), wantSince)
		}
		if q.Get("sort") != "created" || q.Get("direction") != "asc" {
			t.Errorf("sort/direction = %q/%q, want the explicit created-ascending declaration", q.Get("sort"), q.Get("direction"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{commentWindowReviewDoc(1, statusTestUser, "2026-10-22T09:00:00Z")})
	})

	items, report := fetchReviewComments(run, statusTestRepo, "o", "r", start, end)

	if *calls != 1 {
		t.Fatalf("requests = %d, want 1 (the day's comments on the first page)", *calls)
	}
	if len(items) != 1 || items[0].Ref != "gh:o/r:pr-review-comment:1" {
		t.Fatalf("retained items = %v, want the requested day's comment", items)
	}
	if report.Status != StatusComplete || report.Count != 1 {
		t.Fatalf("family report = %+v, want complete review-comment count 1", report)
	}
}

func TestCommentWalkStopsAfterFullPagePastWindow(t *testing.T) {
	start, end := statusTestWindow, powWindowEnd()
	t.Run("mixed page is processed then stops", func(t *testing.T) {
		run, calls := commentWindowRun(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "1" {
				docs := make([]map[string]any, 100)
				docs[0] = commentWindowReviewDoc(1, statusTestUser, "2026-10-22T10:00:00Z")
				for i := 1; i < 100; i++ {
					docs[i] = commentWindowReviewDoc(1000+i, "other-actor", "2026-10-23T00:00:00Z")
				}
				_ = json.NewEncoder(w).Encode(docs)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{commentWindowReviewDoc(2, statusTestUser, "2026-10-22T11:00:00Z")})
		})

		items, report := fetchReviewComments(run, statusTestRepo, "o", "r", start, end)

		if *calls != 1 {
			t.Fatalf("requests = %d, want 1: a full page ending past windowEnd proves later pages are too", *calls)
		}
		if len(items) != 1 || items[0].Ref != "gh:o/r:pr-review-comment:1" {
			t.Fatalf("retained items = %v, want the in-window item from the stopped page", items)
		}
		if report.Status != StatusComplete {
			t.Fatalf("family status = %q, want complete", report.Status)
		}
		if !strings.Contains(report.Detail, "window end reached on page 1") {
			t.Fatalf("detail %q must carry the window-end proof", report.Detail)
		}
	})
	t.Run("post-window page stops before user filtering", func(t *testing.T) {
		run, calls := commentWindowRun(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "1" {
				docs := make([]map[string]any, 100)
				for i := range docs {
					docs[i] = commentWindowReviewDoc(2000+i, "other-actor", "2026-10-23T00:00:00Z")
				}
				_ = json.NewEncoder(w).Encode(docs)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{commentWindowReviewDoc(3, statusTestUser, "2026-10-22T11:00:00Z")})
		})

		items, report := fetchReviewComments(run, statusTestRepo, "o", "r", start, end)

		if *calls != 1 || len(items) != 0 {
			t.Fatalf("requests/items = %d/%d, want 1/0: the stop is decided before user filtering", *calls, len(items))
		}
		if report.Status != "empty" || !strings.Contains(report.Detail, "window end reached on page 1") {
			t.Fatalf("family report = %+v, want empty with the window-end proof", report)
		}
	})
}

func TestCommentWalkMalformedTimestampDoesNotProveWindowEnd(t *testing.T) {
	start, end := statusTestWindow, powWindowEnd()
	run, calls := commentWindowRun(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			docs := make([]map[string]any, 100)
			docs[0] = commentWindowReviewDoc(1, statusTestUser, "not-a-timestamp")
			for i := 1; i < 100; i++ {
				docs[i] = commentWindowReviewDoc(3000+i, statusTestUser, "2026-10-23T00:00:00Z")
			}
			_ = json.NewEncoder(w).Encode(docs)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{commentWindowReviewDoc(2, statusTestUser, "2026-10-22T12:00:00Z")})
	})

	items, report := fetchReviewComments(run, statusTestRepo, "o", "r", start, end)

	if *calls != 2 {
		t.Fatalf("requests = %d, want 2: a page with an unparseable created_at proves nothing", *calls)
	}
	if len(items) != 1 || items[0].Ref != "gh:o/r:pr-review-comment:2" {
		t.Fatalf("retained items = %v, want the day's comment from the next page", items)
	}
	if strings.Contains(report.Detail, "window end reached") {
		t.Fatalf("detail %q must not claim window coverage after a malformed timestamp", report.Detail)
	}
}

func TestCommentWalkCeilingStaysIncompleteWithoutPageClaim(t *testing.T) {
	start, end := statusTestWindow, powWindowEnd()
	nextID := 4000
	run, calls := commentWindowRun(t, func(w http.ResponseWriter, r *http.Request) {
		docs := make([]map[string]any, 100)
		for i := range docs {
			docs[i] = commentWindowReviewDoc(nextID, statusTestUser, "2026-10-22T10:00:00Z")
			nextID++
		}
		_ = json.NewEncoder(w).Encode(docs)
	})

	items, report := fetchReviewComments(run, statusTestRepo, "o", "r", start, end)

	if *calls != maxGithubFamilyPages {
		t.Fatalf("requests = %d, want the %d-page ceiling", *calls, maxGithubFamilyPages)
	}
	if len(items) != maxGithubFamilyPages*100 {
		t.Fatalf("retained items = %d, want %d", len(items), maxGithubFamilyPages*100)
	}
	if report.Status != StatusIncomplete {
		t.Fatalf("family status = %q, want %q at a genuine ceiling", report.Status, StatusIncomplete)
	}
	if !strings.Contains(report.Detail, fmt.Sprintf("coverage not established beyond the %d page(s) examined", maxGithubFamilyPages)) {
		t.Fatalf("detail %q must say coverage is not established at the ceiling", report.Detail)
	}
	if strings.Contains(report.Detail, "more pages available") {
		t.Fatalf("detail %q must not assert more pages definitely exist", report.Detail)
	}
}

func TestCommentWindowStrictStartInLocalTimezone(t *testing.T) {
	loc := time.FixedZone("UTC+05:30", 5*3600+1800)
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, loc)
	end := start.Add(24 * time.Hour)
	wantSince := start.Add(-time.Minute).UTC().Format(time.RFC3339)
	run, calls := commentWindowRun(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != wantSince {
			t.Errorf("since = %q, want %q (local midnight pulled back in UTC)", got, wantSince)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			commentWindowReviewDoc(1, statusTestUser, start.Add(-500*time.Millisecond).UTC().Format(time.RFC3339Nano)),
			commentWindowReviewDoc(2, statusTestUser, start.UTC().Format(time.RFC3339)),
			commentWindowReviewDoc(3, statusTestUser, end.Add(-time.Second).UTC().Format(time.RFC3339)),
			commentWindowReviewDoc(4, statusTestUser, end.UTC().Format(time.RFC3339)),
			commentWindowReviewDoc(5, "other-actor", start.Add(time.Hour).UTC().Format(time.RFC3339)),
		})
	})

	items, report := fetchReviewComments(run, statusTestRepo, "o", "r", start, end)

	if *calls != 1 {
		t.Fatalf("requests = %d, want 1", *calls)
	}
	if len(items) != 2 || items[0].Ref != "gh:o/r:pr-review-comment:2" || items[1].Ref != "gh:o/r:pr-review-comment:3" {
		t.Fatalf("retained items = %v, want only [local start, end) items by the configured user", items)
	}
	if report.Status != StatusComplete || report.Count != 2 {
		t.Fatalf("family report = %+v, want complete count 2", report)
	}
}

func TestIssueCommentQueryUsesSameSinceOverlap(t *testing.T) {
	start, end := statusTestWindow, powWindowEnd()
	wantSince := start.Add(-time.Minute).UTC().Format(time.RFC3339)
	run, calls := commentWindowRun(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/issues/comments") {
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7})
			return
		}
		q := r.URL.Query()
		if q.Get("since") != wantSince {
			t.Errorf("since = %q, want %q", q.Get("since"), wantSince)
		}
		if q.Get("sort") != "created" || q.Get("direction") != "asc" {
			t.Errorf("sort/direction = %q/%q, want created ascending", q.Get("sort"), q.Get("direction"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			commentWindowIssueDoc(1, 7, statusTestUser, "2026-10-22T09:00:00Z"),
			commentWindowIssueDoc(2, 8, "other-actor", "2026-10-22T09:30:00Z"),
		})
	})

	items, report := fetchIssueComments(run, statusTestRepo, "o", "r", start, end)

	if *calls != 2 {
		t.Fatalf("requests = %d, want 2 (comments page plus the PR discriminator lookup)", *calls)
	}
	if len(items) != 1 || items[0].Ref != "gh:o/r:issue-comment:1" {
		t.Fatalf("retained items = %v, want only the configured user's comment", items)
	}
	if report.Status != StatusComplete || report.Count != 1 {
		t.Fatalf("family report = %+v, want complete count 1", report)
	}
}

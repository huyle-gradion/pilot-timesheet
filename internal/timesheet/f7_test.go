package timesheet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSubmittedReviewFailureRetainsVerifiedReviews(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count":        2,
			"incomplete_results": false,
			"items": []map[string]any{
				{"number": 7, "title": "synthetic first candidate"},
				{"number": 9, "title": "synthetic failing candidate"},
			},
		})
	})
	mux.HandleFunc("/repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id":           707,
			"user":         map[string]any{"login": "reviewer"},
			"state":        "APPROVED",
			"submitted_at": "2026-10-22T14:00:00Z",
			"html_url":     "https://example.invalid/o/r/pull/7#pullrequestreview-707",
		}})
	})
	mux.HandleFunc("/repos/o/r/pulls/9/reviews", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic provider failure", http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse synthetic base URL: %v", err)
	}
	run := &githubRun{
		scope:  &githubScope{user: "reviewer", repos: []string{"o/r"}, apiBase: base, token: "synthetic"},
		client: srv.Client(),
	}
	windowStart := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(24 * time.Hour)

	items, report := fetchSubmittedReviews(run, "o/r", "o", "r", windowStart, windowEnd)

	if len(items) != 1 {
		t.Fatalf("retained items = %d, want 1 (%v)", len(items), items)
	}
	if items[0].Ref != "gh:o/r:review:707" {
		t.Fatalf("retained ref = %q, want %q", items[0].Ref, "gh:o/r:review:707")
	}
	if report.Count != len(items) {
		t.Fatalf("report count = %d, retained items = %d; count must match retained evidence", report.Count, len(items))
	}
	if report.Status != StatusFailed {
		t.Fatalf("report status = %q, want %q", report.Status, StatusFailed)
	}
	if !strings.Contains(report.Detail, "HTTP 503") {
		t.Fatalf("report detail %q does not name the provider failure", report.Detail)
	}
	if !strings.Contains(report.Detail, "unverified") {
		t.Fatalf("report detail %q does not mark remaining coverage unverified", report.Detail)
	}
}

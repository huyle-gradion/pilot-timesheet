package timesheet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// overflowEventsHandler serves n attributable WatchEvents inside the status
// test window, 100 per page; failPage > 0 answers that page with a 422 so the
// events family reports limited coverage after the earlier pages.
func overflowEventsHandler(n, failPage int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		if failPage > 0 && page >= failPage {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		docs := []map[string]any{}
		for i := (page - 1) * 100; i < page*100 && i < n; i++ {
			at := statusTestWindow.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
			docs = append(docs, syntheticGithubEvent(i, statusTestUser, at))
		}
		_ = json.NewEncoder(w).Encode(docs)
	}
}

func collectOverflowFixture(t *testing.T, f *githubStatusFixture) collectGithubResult {
	t.Helper()
	return collectGithub(&githubScope{
		user: statusTestUser, repos: []string{statusTestRepo},
		apiBase: mustParseURL(t, f.server.URL), token: "synthetic-token",
	}, statusTestWindow, statusTestWindow.Add(24*time.Hour))
}

func TestCollectGithubRetainedOverflowIsComplete(t *testing.T) {
	got := collectOverflowFixture(t, newGithubStatusFixture(t, overflowEventsHandler(56, 0), nil))
	if got.Status != StatusComplete {
		t.Fatalf("status = %q (%s), want complete: retained overflow is summarized, not missing", got.Status, got.Detail)
	}
	want := fmt.Sprintf("6 attributable item(s) beyond the %d-item topic cap are summarized in the draft", maxTopicItems)
	if !strings.Contains(got.Detail, want) {
		t.Fatalf("detail %q must note the summarized overflow", got.Detail)
	}
}

func TestCollectGithubBoundedFamilyWithOverflowStaysIncomplete(t *testing.T) {
	got := collectOverflowFixture(t, newGithubStatusFixture(t, overflowEventsHandler(156, 2), nil))
	if got.Status != StatusIncomplete {
		t.Fatalf("status = %q (%s), want incomplete: a limited family still bounds the source", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "family queries reported limited or failed coverage") {
		t.Fatalf("detail %q must keep naming the limited family", got.Detail)
	}
}

func TestBuildDraftSummarizesEachOverflowingTopicOnItsOwnLine(t *testing.T) {
	mk := func(title, kind string, kept int, first, last string) TopicCandidate {
		var overflow []EvidenceItem
		for i := 0; i < kept; i++ {
			at := first
			if i == kept-1 {
				at = last
			}
			overflow = append(overflow, overflowFixtureItem(i, kind, at, "acme/api", ""))
		}
		return TopicCandidate{
			Title: title,
			Items: []EvidenceItem{{
				Ref: "ev-" + title, Kind: kind, Time: "2026-10-26T10:00:00+07:00", Repo: "acme/api",
			}},
			OverflowItems: overflow,
			Truncated:     true,
		}
	}
	topics := []TopicCandidate{
		mk("Other attributable activity (acme/api)", "event", 6, "2026-10-26T02:07:00+07:00", "2026-10-26T09:40:00+07:00"),
		mk("Commits (acme/api)", "commit", 3, "2026-10-26T11:00:00+07:00", "2026-10-26T12:15:00+07:00"),
	}
	eb := evidenceBlock(t, BuildDraft(draftReview(nil, nil, topics, nil), "test"))
	want := []string{
		"Other attributable activity (acme/api) — overflow: +6 more items not listed (event 6), 02:07–09:40",
		"Commits (acme/api) — overflow: +3 more items not listed (commit 3), 11:00–12:15",
	}
	if strings.Join(eb.TopicCandidates, "\n") != strings.Join(want, "\n") {
		t.Fatalf("topic_candidates = %q, want one summary line per overflowing topic %q", eb.TopicCandidates, want)
	}
}

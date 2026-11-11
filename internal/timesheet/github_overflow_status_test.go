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

func TestCollectGithubLostOverflowStaysIncomplete(t *testing.T) {
	// 50 listed + 150 retained overflow + 10 beyond the per-topic ceiling.
	got := collectOverflowFixture(t, newGithubStatusFixture(t, overflowEventsHandler(210, 0), nil))
	if got.Status != StatusIncomplete {
		t.Fatalf("status = %q (%s), want incomplete: lost items are missing coverage", got.Status, got.Detail)
	}
	want := fmt.Sprintf("160 attributable item(s) beyond the %d-item topic cap: 150 retained durably as topic overflow, 10 beyond the overflow caps are not retained", maxTopicItems)
	if !strings.Contains(got.Detail, want) {
		t.Fatalf("detail %q must name the retained and lost counts", got.Detail)
	}
	if len(got.Topics) != 1 || got.Topics[0].DroppedItems != 10 || len(got.Topics[0].OverflowItems) != 150 {
		t.Fatalf("topics = %d, want one topic with 150 retained and 10 lost", len(got.Topics))
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
	d := BuildDraft(draftReview(nil, nil, topics, nil), "test")
	var summaries []string
	for _, b := range d.Blocks {
		for _, line := range b.TopicCandidates {
			if strings.Contains(line, "overflow") {
				summaries = append(summaries, line)
			}
		}
	}
	want := []string{
		"Other attributable activity (acme/api) — overflow: +6 more items (event 6), 02:07–09:40",
		"Commits (acme/api) — overflow: +3 more items (commit 3), 11:00–12:15",
	}
	if strings.Join(summaries, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summary lines = %q, want exactly one per overflowing topic %q", summaries, want)
	}
}

func TestPartitionDayOverflowShapesBlocksButIsNotListed(t *testing.T) {
	listed := func(n int) []EvidenceItem {
		var items []EvidenceItem
		for i := 0; i < n; i++ {
			items = append(items, overflowFixtureItem(i, "event",
				fmt.Sprintf("2026-10-26T00:%02d:00+07:00", 10+i), "acme/api", ""))
		}
		return items
	}
	base := TopicCandidate{Title: "Other attributable activity (acme/api)", Items: listed(50)}
	withOverflow := base
	for i, at := range []string{"02:07", "03:00", "04:00", "06:00", "08:00", "09:40"} {
		withOverflow.OverflowItems = append(withOverflow.OverflowItems,
			overflowFixtureItem(100+i, "event", "2026-10-26T"+at+":00+07:00", "acme/api", ""))
	}
	withOverflow.Truncated = true

	plain := BuildDraft(draftReview(nil, nil, []TopicCandidate{base}, nil), "test")
	got := BuildDraft(draftReview(nil, nil, []TopicCandidate{withOverflow}, nil), "test")

	covered := func(at string) bool {
		for _, b := range got.Blocks {
			if b.Kind == "evidence" && b.Start != "" && b.Start <= at && at <= b.End {
				return true
			}
		}
		return false
	}
	for _, at := range []string{"02:07", "03:00", "04:00", "06:00", "08:00", "09:40"} {
		if !covered(at) {
			t.Fatalf("overflow instant %s is in no evidence block: %+v", at, got.Blocks)
		}
	}
	listedSources := func(d Draft) int {
		n := 0
		for _, b := range d.Blocks {
			n += len(b.Sources)
		}
		return n
	}
	if a, b := listedSources(plain), listedSources(got); a != b {
		t.Fatalf("listed sources = %d with overflow, want %d: overflow must stay a summary", b, a)
	}
	again, _ := json.Marshal(BuildDraft(draftReview(nil, nil, []TopicCandidate{withOverflow}, nil), "test"))
	first, _ := json.Marshal(got)
	if string(first) != string(again) {
		t.Fatalf("draft is not deterministic:\nfirst:  %s\nsecond: %s", first, again)
	}
}

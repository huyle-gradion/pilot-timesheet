package timesheet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func powWindowEnd() time.Time { return statusTestWindow.Add(24 * time.Hour) }

func powActor() map[string]any { return map[string]any{"login": statusTestUser} }

func powEvent(id, eventType, at string, payload map[string]any) map[string]any {
	doc := map[string]any{
		"id":         id,
		"type":       eventType,
		"actor":      powActor(),
		"created_at": at,
	}
	if payload != nil {
		doc["payload"] = payload
	}
	return doc
}

func powEventsHandler(events []map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(events)
	}
}

func powIssueComment(id, issueNumber int, at string) map[string]any {
	return map[string]any{
		"id":         id,
		"created_at": at,
		"issue_url":  fmt.Sprintf("https://api.github.invalid/repos/o/r/issues/%d", issueNumber),
		"html_url":   fmt.Sprintf("https://example.invalid/o/r/issues/%d#issuecomment-%d", issueNumber, id),
		"body":       "synthetic comment",
		"user":       powActor(),
	}
}

func powFindItem(items []EvidenceItem, ref string) *EvidenceItem {
	for i := range items {
		if items[i].Ref == ref {
			return &items[i]
		}
	}
	return nil
}

func TestFetchMergedPullsAttributesVerifiedMerge(t *testing.T) {
	f := newPowMergeFixture(t, nil, powMergeSearchDoc(1, false,
		powMergeDetail(12, "2026-10-22T10:00:00Z", statusTestUser)))

	items, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if len(items) != 1 {
		t.Fatalf("retained items = %d, want 1 (%v)", len(items), items)
	}
	item := items[0]
	if item.Ref != "gh:o/r:pr-merge:12" || item.Kind != "merged_pull_request" ||
		item.Resource != "pr:o/r:12" || item.Actor != statusTestUser ||
		item.Time != "2026-10-22T10:00:00Z" || item.Provenance != "search/issues,repos/pulls/12" {
		t.Fatalf("merge item identity = %+v, want the pinned canonical merged_pull_request item", item)
	}
	if item.URL != "https://example.invalid/o/r/pull/12" || item.Title != "synthetic merge" {
		t.Fatalf("merge item presentation = %+v", item)
	}
	if report.Family != "merged_pull_requests" || report.Count != 1 || report.Status != StatusComplete {
		t.Fatalf("family report = %+v, want complete merged_pull_requests with count 1", report)
	}
}

func TestFetchMergedPullsIncludesWindowStart(t *testing.T) {
	f := newPowMergeFixture(t, nil, powMergeSearchDoc(1, false,
		powMergeDetail(12, statusTestWindow.UTC().Format(time.RFC3339), statusTestUser)))

	items, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if len(items) != 1 || items[0].Ref != "gh:o/r:pr-merge:12" {
		t.Fatalf("retained items = %v, want the merge at the inclusive window start", items)
	}
	if report.Status != StatusComplete || report.Count != 1 {
		t.Fatalf("family report = %+v, want complete with count 1", report)
	}
}

func TestFetchMergedPullsDropsMergeByOtherActor(t *testing.T) {
	f := newPowMergeFixture(t, nil, powMergeSearchDoc(1, false,
		powMergeDetail(12, "2026-10-22T10:00:00Z", "other-actor")))

	items, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if len(items) != 0 {
		t.Fatalf("retained items = %d, want 0: another actor's merge is never attributed (%v)", len(items), items)
	}
	if report.Count != 0 || report.Status != "empty" {
		t.Fatalf("family report = %+v, want empty with count 0", report)
	}
}

func TestFetchMergedPullsDropsMergeOutsideWindow(t *testing.T) {
	before := statusTestWindow.Add(-time.Second).UTC().Format(time.RFC3339)
	f := newPowMergeFixture(t, nil, powMergeSearchDoc(1, false,
		powMergeDetail(11, before, statusTestUser)))

	items, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if len(items) != 0 {
		t.Fatalf("retained items = %d, want 0: [start, end) is the day window (%v)", len(items), items)
	}
	if report.Count != 0 {
		t.Fatalf("family count = %d, want 0", report.Count)
	}
}

func TestFetchMergedPullsDropsMergeBeforeWindowStartBySubsecond(t *testing.T) {
	before := statusTestWindow.Add(-500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	f := newPowMergeFixture(t, nil, powMergeSearchDoc(1, false,
		powMergeDetail(12, before, statusTestUser)))

	items, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if len(items) != 0 {
		t.Fatalf("retained items = %d, want 0: merged_at 500ms before windowStart is outside [start, end) (%v)", len(items), items)
	}
	if report.Count != 0 {
		t.Fatalf("family count = %d, want 0", report.Count)
	}
}

func TestFetchMergedPullsDropsMergeAtWindowEnd(t *testing.T) {
	f := newPowMergeFixture(t, nil, powMergeSearchDoc(1, false,
		powMergeDetail(12, powWindowEnd().UTC().Format(time.RFC3339), statusTestUser)))

	items, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if len(items) != 0 {
		t.Fatalf("retained items = %d, want 0: windowEnd is exclusive (%v)", len(items), items)
	}
	if report.Count != 0 {
		t.Fatalf("family count = %d, want 0", report.Count)
	}
}

func TestFetchMergedPullsUnverifiedResultsAreReported(t *testing.T) {
	f := newPowMergeFixture(t, nil, powMergeSearchDoc(2, false,
		powMergeDetail(11, "2026-10-22T10:00:00Z", ""),
		powMergeDetail(12, "", statusTestUser)))

	items, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if len(items) != 0 {
		t.Fatalf("retained items = %d, want 0: unverified merges are never attributed (%v)", len(items), items)
	}
	if report.Status != StatusIncomplete {
		t.Fatalf("family status = %q, want %q", report.Status, StatusIncomplete)
	}
	if !strings.Contains(report.Detail, "2 merged pull request search result(s)") {
		t.Fatalf("detail %q must report both unverified results", report.Detail)
	}

	other := newPowMergeFixture(t, nil, powMergeSearchDoc(1, false,
		powMergeDetail(13, "", "other-actor")))
	items, report = fetchMergedPulls(other.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())
	if len(items) != 0 {
		t.Fatalf("retained items = %d, want 0: another actor's merge is never attributed (%v)", len(items), items)
	}
	if report.Status != "empty" {
		t.Fatalf("family status = %q (%s), want empty: another actor's unverifiable merge is not a coverage bound", report.Status, report.Detail)
	}
}

func TestMergedPullRequestRESTAndEventDeduplicate(t *testing.T) {
	f := newPowMergeFixture(t,
		powEventsHandler([]map[string]any{powEvent("evt-merge-12", "PullRequestEvent", "2026-10-22T10:00:07Z", map[string]any{
			"action": "closed",
			"pull_request": map[string]any{
				"number":    12,
				"title":     "synthetic merge",
				"merged":    true,
				"merged_at": "2026-10-22T10:00:00Z",
				"merged_by": powActor(),
				"html_url":  "https://example.invalid/o/r/pull/12",
			},
		})}),
		powMergeSearchDoc(1, false,
			powMergeDetail(12, "2026-10-22T10:00:00Z", statusTestUser)))

	run := f.run(t)
	merges, _ := fetchMergedPulls(run, statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())
	feed, _ := fetchRepoEvents(run, statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())
	topics := buildGithubTopics(append(merges, feed...))

	if len(topics) != 1 {
		t.Fatalf("topics = %d, want 1 (one merge, one PR group): %+v", len(topics), topics)
	}
	if len(topics[0].Items) != 1 {
		t.Fatalf("topic items = %d, want 1 after canonical-identity deduplication", len(topics[0].Items))
	}
	item := topics[0].Items[0]
	if item.Kind != "merged_pull_request" || item.Ref != "gh:o/r:pr-merge:12" || item.Resource != "pr:o/r:12" {
		t.Fatalf("deduplicated item = %+v, want the pinned merge identity", item)
	}
	if item.Provenance != "search/issues,repos/pulls/12,repos/events" {
		t.Fatalf("provenance = %q, want the REST and events provenances merged", item.Provenance)
	}
	if topics[0].Title != "Pull request #12 (o/r)" {
		t.Fatalf("topic title = %q, want PR activity", topics[0].Title)
	}
}

func TestFetchMergedPullsSearchBoundsForceIncomplete(t *testing.T) {
	t.Run("incomplete_results", func(t *testing.T) {
		f := newPowMergeFixture(t, nil, powMergeSearchDoc(0, true))
		_, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())
		if report.Status != StatusIncomplete || !strings.Contains(report.Detail, "incomplete_results") {
			t.Fatalf("family report = %+v, want incomplete naming incomplete_results", report)
		}
	})

	t.Run("total_count_above_1000", func(t *testing.T) {
		f := newPowMergeFixture(t, nil, powMergeSearchDoc(1001, false))
		_, report := fetchMergedPulls(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())
		if report.Status != StatusIncomplete || !strings.Contains(report.Detail, "1,000-result bound") {
			t.Fatalf("family report = %+v, want incomplete naming the 1,000-result bound", report)
		}
	})
}

func TestIssueCommentClassificationSeparatesPRFromIssue(t *testing.T) {
	f := newGithubStatusFixture(t, nil, nil)
	f.issueCommentsHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			powIssueComment(100, 5, "2026-10-22T09:00:00Z"),
			powIssueComment(101, 7, "2026-10-22T09:05:00Z"),
		})
	}
	f.issueLookupHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/5"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":       5,
				"pull_request": map[string]any{"url": "https://api.github.invalid/repos/o/r/pulls/5"},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7})
		}
	}

	items, report := fetchIssueComments(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if len(items) != 2 {
		t.Fatalf("retained items = %d, want 2 (%v)", len(items), items)
	}
	pr := powFindItem(items, "gh:o/r:issue-comment:100")
	if pr == nil || pr.Kind != "pr_conversation_comment" || pr.Resource != "pr:o/r:5" {
		t.Fatalf("PR conversation comment = %+v, want pr_conversation_comment + pr:o/r:5", pr)
	}
	issue := powFindItem(items, "gh:o/r:issue-comment:101")
	if issue == nil || issue.Kind != "issue_comment" || issue.Resource != "issue:o/r:7" {
		t.Fatalf("plain issue comment = %+v, want issue_comment + issue:o/r:7", issue)
	}
	if report.Family != "issue_comments" || report.Status != StatusComplete || report.Count != 2 {
		t.Fatalf("family report = %+v, want complete issue_comments with count 2", report)
	}
	if *f.issueLookupCalls != 2 {
		t.Fatalf("issue lookups = %d, want 2 (one per distinct number)", *f.issueLookupCalls)
	}
}

func TestIssueCommentLookupFailureStaysIssueAndMarksIncomplete(t *testing.T) {
	f := newGithubStatusFixture(t, nil, nil)
	f.issueCommentsHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{powIssueComment(200, 9, "2026-10-22T09:00:00Z")})
	}
	f.issueLookupHandler = func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic lookup failure", http.StatusServiceUnavailable)
	}

	items, report := fetchIssueComments(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if len(items) != 1 {
		t.Fatalf("retained items = %d, want 1 (%v)", len(items), items)
	}
	if items[0].Kind != "issue_comment" || items[0].Resource != "issue:o/r:9" {
		t.Fatalf("failed lookup item = %+v, want the issue-comment fallback, never a guess", items[0])
	}
	if report.Status != StatusIncomplete {
		t.Fatalf("family status = %q, want %q", report.Status, StatusIncomplete)
	}
	if !strings.Contains(report.Detail, "1 comment(s)") || !strings.Contains(report.Detail, "never guessed") {
		t.Fatalf("detail %q must name the unclassified count and the no-guess rule", report.Detail)
	}
}

func TestIssueCommentLookupCeilingBoundsRequests(t *testing.T) {
	f := newGithubStatusFixture(t, nil, nil)
	f.issueCommentsHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		comments := make([]map[string]any, 0, maxIssueTargetLookups+10)
		for i := 0; i < maxIssueTargetLookups+10; i++ {
			comments = append(comments, powIssueComment(300+i, 1000+i, "2026-10-22T09:00:00Z"))
		}
		_ = json.NewEncoder(w).Encode(comments)
	}
	f.issueLookupHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}

	items, report := fetchIssueComments(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if *f.issueLookupCalls != maxIssueTargetLookups {
		t.Fatalf("issue lookups = %d, want the %d-lookup ceiling (no unbounded requests)", *f.issueLookupCalls, maxIssueTargetLookups)
	}
	if len(items) != maxIssueTargetLookups+10 || report.Count != len(items) {
		t.Fatalf("retained items = %d (report %d), want all %d", len(items), report.Count, maxIssueTargetLookups+10)
	}
	for _, item := range items {
		if item.Kind != "issue_comment" {
			t.Fatalf("item %+v must stay an issue comment: the ceiling is never a guess", item)
		}
	}
	if report.Status != StatusIncomplete {
		t.Fatalf("family status = %q, want %q", report.Status, StatusIncomplete)
	}
	if !strings.Contains(report.Detail, "10 comment(s)") {
		t.Fatalf("detail %q must report the 10 comments beyond the lookup ceiling", report.Detail)
	}
}

func TestPRTopicGroupsOpenMergeAndConversationComment(t *testing.T) {
	search := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("q")
		switch {
		case strings.Contains(q, "author:"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1, "incomplete_results": false,
				"items": []map[string]any{{
					"number": 12, "title": "Open PR", "created_at": "2026-10-22T09:00:00Z",
					"html_url": "https://example.invalid/o/r/pull/12",
					"user":     powActor(),
				}},
			})
		case strings.Contains(q, "merged:"):
			powMergeSearchHandler(t, powMergeSearchDoc(1, false,
				powMergeDetail(12, "2026-10-22T15:00:00Z", statusTestUser)))(w, r)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "incomplete_results": false, "items": []map[string]any{}})
		}
	}
	f := newGithubStatusFixture(t, powEventsHandler([]map[string]any{}), search)
	registerPowMergeDetails(t, f, []map[string]any{powMergeDetail(12, "2026-10-22T15:00:00Z", statusTestUser)})
	f.issueCommentsHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			powIssueComment(101, 12, "2026-10-22T11:00:00Z"),
			powIssueComment(102, 99, "2026-10-22T12:00:00Z"),
		})
	}
	f.issueLookupHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/99") {
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 99})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"pull_request": map[string]any{"url": "https://api.github.invalid/pulls"}})
	}

	got := collectGithub(&githubScope{
		user:    statusTestUser,
		repos:   []string{statusTestRepo},
		apiBase: mustParseURL(t, f.server.URL),
		token:   "synthetic-token",
	}, statusTestWindow, powWindowEnd())

	var prTopic, issueTopic *TopicCandidate
	for i := range got.Topics {
		switch got.Topics[i].Title {
		case "Pull request #12 (o/r)":
			prTopic = &got.Topics[i]
		case "Issue #99 activity (o/r)":
			issueTopic = &got.Topics[i]
		case "Issue #12 activity (o/r)":
			t.Fatalf("PR #12 conversation comment must not form an issue topic: %+v", got.Topics[i])
		}
	}
	if prTopic == nil {
		t.Fatalf("missing PR #12 topic in %+v", got.Topics)
	}
	if len(prTopic.Items) != 3 {
		t.Fatalf("PR topic items = %d, want 3 (open, merge, conversation comment): %+v", len(prTopic.Items), prTopic.Items)
	}
	wantKinds := map[string]bool{"authored_pull_request": false, "merged_pull_request": false, "pr_conversation_comment": false}
	for _, item := range prTopic.Items {
		if item.Resource != "pr:o/r:12" {
			t.Fatalf("item %+v joined the PR topic with another resource", item)
		}
		if _, ok := wantKinds[item.Kind]; !ok {
			t.Fatalf("unexpected kind %q in the PR topic", item.Kind)
		}
		wantKinds[item.Kind] = true
	}
	for kind, seen := range wantKinds {
		if !seen {
			t.Fatalf("PR topic is missing the %s shape", kind)
		}
	}
	if issueTopic == nil || len(issueTopic.Items) != 1 || issueTopic.Items[0].Kind != "issue_comment" {
		t.Fatalf("issue comment topic = %+v, want one issue_comment item", issueTopic)
	}
}

func TestFetchRepoEventsMergeRechecksMergedAt(t *testing.T) {
	events := []map[string]any{
		powEvent("evt-early-merge", "PullRequestEvent", "2026-10-22T00:00:01Z", map[string]any{
			"action": "closed",
			"pull_request": map[string]any{
				"number": 21, "title": "Previous-day merge", "merged": true,
				"merged_at": "2026-10-21T23:59:59Z", "merged_by": powActor(),
				"html_url": "https://example.invalid/o/r/pull/21",
			},
		}),
		powEvent("evt-windowed-merge", "PullRequestEvent", "2026-10-22T10:00:07Z", map[string]any{
			"action": "closed",
			"pull_request": map[string]any{
				"number": 22, "title": "Same-day merge", "merged": true,
				"merged_at": "2026-10-22T10:00:00Z", "merged_by": powActor(),
				"html_url": "https://example.invalid/o/r/pull/22",
			},
		}),
	}
	f := newGithubStatusFixture(t, powEventsHandler(events), nil)

	items, report := fetchRepoEvents(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	if stray := powFindItem(items, "gh:o/r:pr-merge:21"); stray != nil {
		t.Fatalf("out-of-window merge was attributed: %+v", stray)
	}
	early := powFindItem(items, "gh:o/r:event:evt-early-merge")
	if early == nil || early.Kind != "event" {
		t.Fatalf("out-of-window merge must fall through to the generic event: %+v", early)
	}
	merge := powFindItem(items, "gh:o/r:pr-merge:22")
	if merge == nil || merge.Kind != "merged_pull_request" || merge.Resource != "pr:o/r:22" {
		t.Fatalf("same-day feed merge = %+v, want merged_pull_request + pr:o/r:22", merge)
	}
	if merge.Time != "2026-10-22T10:00:00Z" {
		t.Fatalf("feed merge time = %q, want the in-window merged_at instant", merge.Time)
	}
	if report.Status != StatusWindowed || report.Count != 2 {
		t.Fatalf("family report = %+v, want windowed with count 2", report)
	}
}

func TestDedupeKeepsPRConversationKindOverFeedIssueComment(t *testing.T) {
	f := newGithubStatusFixture(t,
		powEventsHandler([]map[string]any{powEvent("evt-comment-500", "IssueCommentEvent", "2026-10-22T09:05:00Z", map[string]any{
			"action":  "created",
			"issue":   map[string]any{"number": 12},
			"comment": map[string]any{"id": 500, "html_url": "https://example.invalid/o/r/pull/12#issuecomment-500"},
		})}),
		nil)
	f.issueCommentsHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{powIssueComment(500, 12, "2026-10-22T09:00:00Z")})
	}
	f.issueLookupHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":       12,
			"pull_request": map[string]any{"url": "https://api.github.invalid/repos/o/r/pulls/12"},
		})
	}

	items, _ := collectRepo(f.run(t), statusTestRepo, statusTestWindow, powWindowEnd())
	topics := buildGithubTopics(items)

	for _, topic := range topics {
		if topic.Title == "Issue #12 activity (o/r)" {
			t.Fatalf("PR conversation comment was downgraded to an issue topic: %+v", topic)
		}
	}
	var one *TopicCandidate
	for i := range topics {
		if topics[i].Title == "Pull request #12 (o/r)" {
			one = &topics[i]
		}
	}
	if one == nil || len(one.Items) != 1 {
		t.Fatalf("PR topic = %+v, want one deduplicated item", one)
	}
	if got := one.Items[0]; got.Kind != "pr_conversation_comment" || got.Resource != "pr:o/r:12" ||
		!strings.Contains(got.Provenance, "issues/comments") || !strings.Contains(got.Provenance, "repos/events") {
		t.Fatalf("deduplicated item = %+v, want PR kind, pr: resource, merged provenance", got)
	}
}

func TestFetchRepoEventsNonMergeTypesUnchanged(t *testing.T) {
	otherPR := map[string]any{
		"number": 5, "title": "Other merge", "merged": true,
		"merged_at": "2026-10-22T09:40:00Z",
		"merged_by": map[string]any{"login": "other-actor"},
		"html_url":  "https://example.invalid/o/r/pull/5",
	}
	closedPR := map[string]any{
		"number": 6, "title": "Closed not merged", "merged": false,
		"merged_by": powActor(),
	}
	events := []map[string]any{
		powEvent("evt-comment", "IssueCommentEvent", "2026-10-22T09:00:00Z", map[string]any{
			"action":  "created",
			"issue":   map[string]any{"number": 3},
			"comment": map[string]any{"id": 300, "html_url": "https://example.invalid/o/r/issues/3#issuecomment-300"},
		}),
		powEvent("evt-review", "PullRequestReviewEvent", "2026-10-22T09:10:00Z", map[string]any{
			"pull_request": map[string]any{"number": 4, "title": "Synthetic PR"},
			"review":       map[string]any{"id": 400, "state": "APPROVED", "html_url": "https://example.invalid/o/r/pull/4#pullrequestreview-400"},
		}),
		powEvent("evt-review-comment", "PullRequestReviewCommentEvent", "2026-10-22T09:20:00Z", map[string]any{
			"pull_request": map[string]any{"number": 4, "title": "Synthetic PR"},
			"comment":      map[string]any{"id": 401, "html_url": "https://example.invalid/o/r/pull/4#discussion_r401"},
		}),
		powEvent("evt-watch", "WatchEvent", "2026-10-22T09:30:00Z", nil),
		powEvent("evt-merge-other", "PullRequestEvent", "2026-10-22T09:40:00Z", map[string]any{
			"action": "closed", "pull_request": otherPR,
		}),
		powEvent("evt-close", "PullRequestEvent", "2026-10-22T09:50:00Z", map[string]any{
			"action": "closed", "pull_request": closedPR,
		}),
	}
	f := newGithubStatusFixture(t, powEventsHandler(events), nil)

	items, report := fetchRepoEvents(f.run(t), statusTestRepo, "o", "r", statusTestWindow, powWindowEnd())

	want := map[string]struct {
		kind     string
		resource string
	}{
		"gh:o/r:issue-comment:300":     {"issue_comment", "issue:o/r:3"},
		"gh:o/r:review:400":            {"submitted_pr_review", "pr:o/r:4"},
		"gh:o/r:pr-review-comment:401": {"pr_review_comment", "pr:o/r:4"},
		"gh:o/r:event:evt-watch":       {"event", ""},
		"gh:o/r:event:evt-merge-other": {"event", ""},
		"gh:o/r:event:evt-close":       {"event", ""},
	}
	if len(items) != len(want) {
		t.Fatalf("retained items = %d, want %d (%v)", len(items), len(want), items)
	}
	for ref, expected := range want {
		item := powFindItem(items, ref)
		if item == nil {
			t.Fatalf("missing event item %q in %v", ref, items)
		}
		if item.Kind != expected.kind || item.Resource != expected.resource {
			t.Fatalf("event item %q = kind %q resource %q, want kind %q resource %q",
				ref, item.Kind, item.Resource, expected.kind, expected.resource)
		}
	}
	if report.Status != StatusWindowed || report.Count != len(items) {
		t.Fatalf("family report = %+v, want windowed with count %d", report, len(items))
	}
}

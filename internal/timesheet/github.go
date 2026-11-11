package timesheet

// Requests carry the configured GITHUB_TOKEN as a Bearer credential; the token
// value never appears in errors, diagnostics, state, or reports, and request
// URLs are never echoed.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type collectGithubResult struct {
	Status   string
	Detail   string
	Families []FamilyReport
	Topics   []TopicCandidate
}

type githubScope struct {
	user    string
	repos   []string
	apiBase *url.URL
	token   string
}

func collectGithub(scope *githubScope, windowStart, windowEnd time.Time) collectGithubResult {
	if strings.TrimSpace(scope.token) == "" {
		return collectGithubResult{
			Status: StatusMissingCredentials,
			Detail: "GITHUB_TOKEN is not set or is empty",
		}
	}
	run := &githubRun{scope: scope, client: &http.Client{Timeout: collectHTTPTimeout}}

	var allItems []EvidenceItem
	var families []FamilyReport
	for _, repo := range scope.repos {
		items, fams := collectRepo(run, repo, windowStart, windowEnd)
		allItems = append(allItems, items...)
		families = append(families, fams...)
	}
	bounded := 0
	for _, fam := range families {
		if fam.Status == StatusIncomplete || fam.Status == StatusFailed {
			bounded++
		}
	}
	topics := buildGithubTopics(allItems)
	overflowKept, overflowLost := 0, 0
	for _, t := range topics {
		overflowKept += len(t.OverflowItems)
		overflowLost += t.DroppedItems
	}
	status, detail := StatusComplete, ""
	if bounded > 0 {
		status = StatusIncomplete
		detail = fmt.Sprintf("%d of %d family queries reported limited or failed coverage", bounded, len(families))
	} else {
		detail = fmt.Sprintf("%d attributable item(s) across %d queried repositories", len(allItems), len(scope.repos))
	}
	if overflowKept > 0 || overflowLost > 0 {
		// The topic item cap bounds the AI-visible draft, not the evidence:
		// items beyond it are retained durably as topic overflow up to the
		// fixed ceilings, and anything beyond those ceilings is a real loss
		// counted here and on the affected topics, never a silent no-op (F6).
		// The source stays incomplete either way: the draft still shows only
		// the first maxTopicItems items per topic.
		status = StatusIncomplete
		if overflowLost > 0 {
			detail = niceJoin(detail, fmt.Sprintf(
				"%d attributable item(s) beyond the %d-item topic cap: %d retained durably as topic overflow, %d beyond the overflow caps are not retained; the affected topic(s) record the counts",
				overflowKept+overflowLost, maxTopicItems, overflowKept, overflowLost))
		} else {
			detail = niceJoin(detail, fmt.Sprintf(
				"%d attributable item(s) beyond the %d-item topic cap are retained durably as topic overflow; the draft still shows the first %d items per topic",
				overflowKept, maxTopicItems, maxTopicItems))
		}
	}
	return collectGithubResult{
		Status:   status,
		Detail:   detail,
		Families: families,
		Topics:   topics,
	}
}

type githubRun struct {
	scope  *githubScope
	client *http.Client
	// issueTargets caches per-run successful issue-document resolutions keyed
	// by "{repo}#{number}", so the issue-comment family can tell a pull request
	// conversation comment from an issue comment with one lookup per distinct
	// number for the whole run.
	issueTargets map[string]issueTarget
}

type issueTarget struct {
	isPR bool
}

type issueLookupBudget struct {
	lookups         int
	failedComments  int
	boundedComments int
}

// issueTargetFor answers whether {owner}/{name}#{number} is a pull request by
// reading the documented issue document, whose response carries a
// "pull_request" key only for a pull request. A cache hit returns without a
// request. At the collector's maxIssueTargetLookups ceiling (per repository
// per attempt) it returns known=false, bounded=true immediately, so a large
// comment family never turns into an unbounded request fan-out. A non-200
// response, transport error, or decode failure returns known=false,
// bounded=false; it never fails the family. The caller keeps such items
// classified as issue comments and reports the counts as incomplete.
func (g *githubRun) issueTargetFor(repo, owner, name string, number int, budget *issueLookupBudget) (isPR, known, bounded bool) {
	key := fmt.Sprintf("%s#%d", repo, number)
	if target, ok := g.issueTargets[key]; ok {
		return target.isPR, true, false
	}
	if budget.lookups >= maxIssueTargetLookups {
		return false, false, true
	}
	budget.lookups++
	target := issueTarget{}
	known = false
	response, err := g.getJSON(nil, fmt.Sprintf("/repos/%s/%s/issues/%d", owner, name, number))
	if err == nil && response.status == http.StatusOK {
		var doc struct {
			PullRequest *json.RawMessage `json:"pull_request"`
		}
		if json.Unmarshal(response.body, &doc) == nil {
			target = issueTarget{isPR: doc.PullRequest != nil}
			known = true
		}
	}
	if known {
		if g.issueTargets == nil {
			g.issueTargets = map[string]issueTarget{}
		}
		g.issueTargets[key] = target
	}
	return target.isPR, known, false
}

type actor struct {
	Login string `json:"login"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type commitDoc struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  *struct {
		Author *struct {
			Name  string `json:"name"`
			Email string `json:"email"`
			Date  string `json:"date"`
		} `json:"author"`
		Message string `json:"message"`
	} `json:"commit"`
	Author *actor `json:"author"`
}

type searchDoc struct {
	TotalCount        int          `json:"total_count"`
	IncompleteResults bool         `json:"incomplete_results"`
	Items             []searchItem `json:"items"`
}

type searchItem struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	HTMLURL   string `json:"html_url"`
	User      *actor `json:"user"`
}

type commentDoc struct {
	ID             int    `json:"id"`
	CreatedAt      string `json:"created_at"`
	HTMLURL        string `json:"html_url"`
	IssueURL       string `json:"issue_url"`
	PullRequestURL string `json:"pull_request_url"`
	Body           string `json:"body"`
	User           *actor `json:"user"`
}

type reviewDoc struct {
	ID          int    `json:"id"`
	State       string `json:"state"`
	SubmittedAt string `json:"submitted_at"`
	HTMLURL     string `json:"html_url"`
	User        *actor `json:"user"`
}

type ghEventDoc struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Actor     *actor            `json:"actor"`
	CreatedAt string            `json:"created_at"`
	Payload   ghEventPayloadDoc `json:"payload"`
}

type ghEventPayloadDoc struct {
	Action     string `json:"action"`
	Discussion *struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	} `json:"discussion"`
	Comment *struct {
		ID      int    `json:"id"`
		HTMLURL string `json:"html_url"`
	} `json:"comment"`
	PullRequest *struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		Merged   bool   `json:"merged"`
		MergedAt string `json:"merged_at"`
		MergedBy *actor `json:"merged_by"`
		HTMLURL  string `json:"html_url"`
	} `json:"pull_request"`
	Issue *struct {
		Number int `json:"number"`
	} `json:"issue"`
	Review *struct {
		ID      int    `json:"id"`
		State   string `json:"state"`
		HTMLURL string `json:"html_url"`
	} `json:"review"`
}

var issueNumberTailRE = regexp.MustCompile(`/issues/(\d+)$`)
var pullNumberTailRE = regexp.MustCompile(`/pulls/(\d+)$`)

func collectRepo(g *githubRun, repo string, windowStart, windowEnd time.Time) ([]EvidenceItem, []FamilyReport) {
	var items []EvidenceItem
	reports := make([]FamilyReport, 0, 7)
	owner, name, _ := strings.Cut(repo, "/")

	commits, rep := fetchCommits(g, repo, owner, name, windowStart, windowEnd)
	items = append(items, commits...)
	reports = append(reports, rep)

	pulls, rep := fetchAuthoredPulls(g, repo, owner, name, windowStart, windowEnd)
	items = append(items, pulls...)
	reports = append(reports, rep)

	merges, rep := fetchMergedPulls(g, repo, owner, name, windowStart, windowEnd)
	items = append(items, merges...)
	reports = append(reports, rep)

	reviews, rep := fetchSubmittedReviews(g, repo, owner, name, windowStart, windowEnd)
	items = append(items, reviews...)
	reports = append(reports, rep)

	reviewComments, rep := fetchReviewComments(g, repo, owner, name, windowStart, windowEnd)
	items = append(items, reviewComments...)
	reports = append(reports, rep)

	issueComments, rep := fetchIssueComments(g, repo, owner, name, windowStart, windowEnd)
	items = append(items, issueComments...)
	reports = append(reports, rep)

	feed, rep := fetchRepoEvents(g, repo, owner, name, windowStart, windowEnd)
	items = append(items, feed...)
	reports = append(reports, rep)

	return items, reports
}

func fetchCommits(g *githubRun, repo, owner, name string, windowStart, windowEnd time.Time) ([]EvidenceItem, FamilyReport) {
	since := windowStart.UTC().Format(time.RFC3339)
	until := windowEnd.UTC().Format(time.RFC3339)
	query := url.Values{
		"author": {g.scope.user},
		"since":  {since},
		"until":  {until},
	}
	path := "/repos/" + owner + "/" + name + "/commits"
	var items []EvidenceItem
	unverified := 0
	decode := func(body []byte) (int, error) {
		var docs []commitDoc
		if err := json.Unmarshal(body, &docs); err != nil {
			return 0, err
		}
		for _, doc := range docs {
			if doc.Commit == nil || doc.Commit.Author == nil || doc.Commit.Author.Date == "" {
				continue
			}
			at, err := time.Parse(time.RFC3339, doc.Commit.Author.Date)
			if err != nil || !at.Before(windowEnd) || !at.After(windowStart.Add(-time.Second)) {
				continue
			}
			if doc.Author == nil || strings.TrimSpace(doc.Author.Login) == "" {
				unverified++
				continue
			}
			if doc.Author.Login != g.scope.user {
				continue
			}
			items = append(items, EvidenceItem{
				Ref:        "gh:commit:" + doc.SHA,
				Kind:       "commit",
				Actor:      doc.Author.Login,
				Time:       at.UTC().Format(time.RFC3339),
				Repo:       repo,
				URL:        doc.HTMLURL,
				Title:      sanitizerText(firstLine(doc.Commit.Message)),
				Provenance: "repos/commits",
			})
		}
		return len(docs), nil
	}
	walk := g.familyPageLoop(query, path, maxGithubFamilyPages, decode)
	n := len(items)
	status, detail := familyStatus(walk, n, "commit")
	if unverified > 0 && status != StatusFailed {
		status = StatusIncomplete
		detail = niceJoin(detail, fmt.Sprintf(
			"%d commit(s) in the day's window carried no GitHub user document; their authorship cannot be verified as the configured user, so they were omitted rather than attributed by commit-author email", unverified))
	}
	return items, FamilyReport{Family: "commits", QueriedRepo: repo, Count: n, Status: status, Detail: detail}
}

// Because `created:` is date-granular, the collector over-fetches the UTC date
// range of the day window and attributes each result by its exact created_at
// instant and its provider-named user.login.
func fetchAuthoredPulls(g *githubRun, repo, owner, name string, windowStart, windowEnd time.Time) ([]EvidenceItem, FamilyReport) {
	dayFrom := windowStart.UTC().Format("2006-01-02")
	dayTo := windowEnd.Add(-time.Second).UTC().Format("2006-01-02")
	query := url.Values{}
	query.Set("q", fmt.Sprintf("type:pr author:%s repo:%s created:%s..%s", g.scope.user, repo, dayFrom, dayTo))
	query.Set("sort", "created")
	query.Set("order", "desc")
	path := "/search/issues"
	var items []EvidenceItem
	incomplete := false
	total := 0
	decode := func(body []byte) (int, error) {
		var doc searchDoc
		if err := json.Unmarshal(body, &doc); err != nil {
			return 0, err
		}
		total = doc.TotalCount
		if doc.IncompleteResults {
			incomplete = true
		}
		for _, item := range doc.Items {
			at, err := time.Parse(time.RFC3339, item.CreatedAt)
			if err != nil || !at.Before(windowEnd) || !at.After(windowStart.Add(-time.Second)) {
				continue
			}
			if item.User == nil || item.User.Login != g.scope.user {
				continue
			}
			items = append(items, EvidenceItem{
				Ref:        fmt.Sprintf("gh:%s:pr:%d", repo, item.Number),
				Kind:       "authored_pull_request",
				Actor:      item.User.Login,
				Time:       at.UTC().Format(time.RFC3339),
				Repo:       repo,
				Resource:   fmt.Sprintf("pr:%s:%d", repo, item.Number),
				URL:        item.HTMLURL,
				Title:      sanitizerText(item.Title),
				Provenance: "search/issues",
			})
		}
		return len(doc.Items), nil
	}
	walk := g.familyPageLoop(query, path, maxSearchResultPages, decode)
	n := len(items)
	status, detail := familyStatus(walk, n, "authored-pull-request search")
	if incomplete {
		status = StatusIncomplete
		detail = niceJoin(detail, "the search provider reported incomplete_results within its documented time/runtime bound")
	}
	if total > 1000 {
		status = StatusIncomplete
		detail = niceJoin(detail, "the search result total exceeds the documented 1,000-result bound")
	}
	if status == StatusComplete && n == 0 {
		status = "empty"
	}
	return items, FamilyReport{Family: "authored_pull_requests", QueriedRepo: repo, Count: n, Status: status, Detail: detail}
}

// Candidate pull requests are a deliberate superset: every PR in the repository
// the user has reviewed, independent of when the resource was last updated,
// because GitHub's search `updated` qualifier means "last updated", not "review
// submitted" — a PR reviewed inside the window and touched later would
// otherwise be excluded. Search bounds, the candidate
// ceiling and the page ceiling make the family incomplete, never silently
// exhausted.
func fetchSubmittedReviews(g *githubRun, repo, owner, name string, windowStart, windowEnd time.Time) ([]EvidenceItem, FamilyReport) {
	query := url.Values{}
	query.Set("q", fmt.Sprintf("type:pr reviewed-by:%s repo:%s", g.scope.user, repo))
	query.Set("sort", "updated")
	query.Set("order", "desc")

	var items []EvidenceItem
	var bounds []string
	var candidates []searchItem
	candTruncated := false
	sawSearchIncomplete := false
	searchCap := false
	searchTotal := 0
	decode := func(body []byte) (int, error) {
		var doc searchDoc
		if err := json.Unmarshal(body, &doc); err != nil {
			return 0, err
		}
		searchTotal = doc.TotalCount
		if doc.IncompleteResults {
			sawSearchIncomplete = true
		}
		for _, item := range doc.Items {
			if len(candidates) >= maxReviewPRCandidates {
				candTruncated = true
				break
			}
			if item.Number == 0 {
				continue
			}
			candidates = append(candidates, item)
		}
		return len(doc.Items), nil
	}
	searchWalk := g.familyPageLoop(query, "/search/issues", maxSearchResultPages, decode)
	if searchWalk.err != nil {
		return nil, FamilyReport{Family: "submitted_pr_reviews", QueriedRepo: repo, Count: 0, Status: StatusFailed, Detail: truncateRunes(searchWalk.err.Error(), maxDetailRunes)}
	}
	if searchWalk.stop == "capped" {
		searchCap = true
	}
	if candTruncated {
		bounds = append(bounds, fmt.Sprintf(
			"the candidate search retained only the first %d pull request(s); the remaining candidate(s) were not verified for submitted reviews", maxReviewPRCandidates))
	}
	if searchCap {
		bounds = append(bounds, fmt.Sprintf(
			"the candidate search reached the collector's %d-page pagination ceiling; more candidate pages exist", maxSearchResultPages))
	}
	if sawSearchIncomplete {
		bounds = append(bounds, "the candidate search reported incomplete_results within its documented time/runtime bound")
	}
	if searchTotal > 1000 {
		bounds = append(bounds, "the candidate search total exceeds the documented 1,000-result bound")
	}

	reviewCapped := false
	for _, cand := range candidates {
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, name, cand.Number)
		decode := func(body []byte) (int, error) {
			var docs []reviewDoc
			if err := json.Unmarshal(body, &docs); err != nil {
				return 0, err
			}
			for _, doc := range docs {
				if doc.User == nil || doc.User.Login != g.scope.user {
					continue
				}
				at, err := time.Parse(time.RFC3339, doc.SubmittedAt)
				if err != nil || !at.Before(windowEnd) || !at.After(windowStart.Add(-time.Second)) {
					continue
				}
				state := strings.TrimSpace(doc.State)
				if state == "" || doc.ID == 0 {
					continue
				}
				items = append(items, EvidenceItem{
					Ref:        fmt.Sprintf("gh:%s:review:%d", repo, doc.ID),
					Kind:       "submitted_pr_review",
					Actor:      doc.User.Login,
					Time:       at.UTC().Format(time.RFC3339),
					Repo:       repo,
					Resource:   fmt.Sprintf("pr:%s:%d", repo, cand.Number),
					URL:        doc.HTMLURL,
					Title:      sanitizerText(fmt.Sprintf("pull request #%d review (%s)", cand.Number, strings.ToLower(state))),
					Provenance: fmt.Sprintf("repos/pulls/%d/reviews", cand.Number),
				})
			}
			return len(docs), nil
		}
		walk := g.familyPageLoop(url.Values{}, path, maxGithubFamilyPages, decode)
		if walk.err != nil {
			detail := niceJoin(walk.err.Error(), fmt.Sprintf(
				"%d submitted review(s) retained before the failure; remaining review coverage is unverified", len(items)))
			return items, FamilyReport{Family: "submitted_pr_reviews", QueriedRepo: repo, Count: len(items), Status: StatusFailed, Detail: truncateRunes(detail, maxDetailRunes)}
		}
		if walk.stop == "capped" {
			reviewCapped = true
		}
	}
	if reviewCapped {
		bounds = append(bounds, fmt.Sprintf(
			"at least one candidate pull request's review list reached the collector's %d-page ceiling; coverage for submitted reviews is bounded", maxGithubFamilyPages))
	}

	n := len(items)
	if len(bounds) > 0 {
		return items, FamilyReport{Family: "submitted_pr_reviews", QueriedRepo: repo, Count: n, Status: StatusIncomplete, Detail: strings.Join(bounds, "; ")}
	}
	status := StatusComplete
	detail := fmt.Sprintf("%d submitted review(s) retained; %d candidate pull request(s) from the update-independent reviewed-by superset verified with the reviews endpoint and filtered by submitted_at; pagination exhausted", n, len(candidates))
	if n == 0 {
		status = "empty"
	}
	return items, FamilyReport{Family: "submitted_pr_reviews", QueriedRepo: repo, Count: n, Status: status, Detail: detail}
}

func fetchReviewComments(g *githubRun, repo, owner, name string, windowStart, windowEnd time.Time) ([]EvidenceItem, FamilyReport) {
	query := url.Values{
		"sort":      {"created"},
		"direction": {"asc"},
		"since":     {commentWindowSince(windowStart)},
	}
	path := "/repos/" + owner + "/" + name + "/pulls/comments"
	resource := func(doc *commentDoc) string {
		return pullResourceFromURL(doc.PullRequestURL, repo)
	}
	items, walk := fetchCommentsPageLoop(g, query, path, repo, windowStart, windowEnd, "pr_review_comment", "pr-review-comment", "pulls/comments", "review-comment", resource, nil)
	n := len(items)
	status, detail := familyStatus(walk, n, "review-comment")
	if status == StatusComplete && n == 0 {
		status = "empty"
	}
	return items, FamilyReport{Family: "pr_review_comments", QueriedRepo: repo, Count: n, Status: status, Detail: detail}
}

func fetchIssueComments(g *githubRun, repo, owner, name string, windowStart, windowEnd time.Time) ([]EvidenceItem, FamilyReport) {
	query := url.Values{
		"sort":      {"created"},
		"direction": {"asc"},
		"since":     {commentWindowSince(windowStart)},
	}
	path := "/repos/" + owner + "/" + name + "/issues/comments"
	resource := func(doc *commentDoc) string {
		return issueResourceFromURL(doc.IssueURL, repo)
	}
	// The issues/comments document has no pull_request field, so a PR
	// conversation comment is indistinguishable from an issue comment in that
	// response.
	budget := &issueLookupBudget{}
	unparsable := 0
	refine := func(item *EvidenceItem, doc *commentDoc) {
		m := issueNumberTailRE.FindStringSubmatch(doc.IssueURL)
		if m == nil {
			unparsable++
			return
		}
		number, err := strconv.Atoi(m[1])
		if err != nil || number == 0 {
			unparsable++
			return
		}
		isPR, known, bounded := g.issueTargetFor(repo, owner, name, number, budget)
		if !known {
			if bounded {
				budget.boundedComments++
			} else {
				budget.failedComments++
			}
			return
		}
		if isPR {
			item.Kind = "pr_conversation_comment"
			item.Resource = fmt.Sprintf("pr:%s:%d", repo, number)
		}
	}
	items, walk := fetchCommentsPageLoop(g, query, path, repo, windowStart, windowEnd, "issue_comment", "issue-comment", "issues/comments", "issue-comment", resource, refine)
	n := len(items)
	status, detail := familyStatus(walk, n, "issue-comment")
	if status != StatusFailed {
		if budget.failedComments > 0 {
			status = StatusIncomplete
			detail = niceJoin(detail, fmt.Sprintf(
				"%d comment(s) could not be verified as pull requests (the documented issue lookup failed or was not HTTP 200); they stay classified as issue comments, never guessed", budget.failedComments))
		}
		if budget.boundedComments > 0 {
			status = StatusIncomplete
			detail = niceJoin(detail, fmt.Sprintf(
				"%d comment(s) exceeded the collector's %d-lookup ceiling per repository per attempt and were not verified as pull requests; they stay classified as issue comments, never guessed", budget.boundedComments, maxIssueTargetLookups))
		}
		if unparsable > 0 {
			status = StatusIncomplete
			detail = niceJoin(detail, fmt.Sprintf(
				"%d comment(s) carried no parsable issue number in issue_url and could not be checked for the documented pull_request discriminator; they stay classified as issue comments, never guessed", unparsable))
		}
	}
	if status == StatusComplete && n == 0 {
		status = "empty"
	}
	return items, FamilyReport{Family: "issue_comments", QueriedRepo: repo, Count: n, Status: status, Detail: detail}
}

func fetchCommentsPageLoop(g *githubRun, query url.Values, path, repo string, windowStart, windowEnd time.Time, kind, refKind, provenancePrefix, label string, resourceFn func(*commentDoc) string, refine func(*EvidenceItem, *commentDoc)) ([]EvidenceItem, familyWalk) {
	var items []EvidenceItem
	window := &commentWindow{windowEnd: windowEnd}
	var pagePassedWindow bool
	decode := func(body []byte) (int, error) {
		var docs []commentDoc
		if err := json.Unmarshal(body, &docs); err != nil {
			return 0, err
		}
		pagePassedWindow = window.observePage(docs)
		for _, doc := range docs {
			if doc.User == nil || doc.User.Login != g.scope.user {
				continue
			}
			at, err := time.Parse(time.RFC3339, doc.CreatedAt)
			if err != nil || at.Before(windowStart) || !at.Before(windowEnd) {
				continue
			}
			title := label
			if strings.EqualFold(label, "review-comment") && doc.PullRequestURL != "" {
				title = "review comment on " + pullRefFromURL(doc.PullRequestURL, repo)
			} else if strings.EqualFold(label, "issue-comment") && doc.IssueURL != "" {
				title = "comment on " + issueRefFromURL(doc.IssueURL, repo)
			}
			item := EvidenceItem{
				Ref:        fmt.Sprintf("gh:%s:%s:%d", repo, refKind, doc.ID),
				Kind:       kind,
				Actor:      doc.User.Login,
				Time:       at.UTC().Format(time.RFC3339),
				Repo:       repo,
				Resource:   resourceFn(&doc),
				URL:        doc.HTMLURL,
				Title:      title,
				Provenance: provenancePrefix,
			}
			if refine != nil {
				refine(&item, &doc)
			}
			items = append(items, item)
		}
		return len(docs), nil
	}
	walk := g.familyPageLoopWindow(query, path, maxGithubFamilyPages, decode, func() bool { return pagePassedWindow })
	walk.coveredFrom, walk.coveredUntil = window.coveredFrom, window.coveredUntil
	return items, walk
}

// GitHub bounds the events timeline by design (only the past 30 days, up to 300
// events per timeline), so the family's steady state is the dedicated
// `windowed` status — honest bounded recent-window coverage, never `empty` and
// never a forced `incomplete`.
func fetchRepoEvents(g *githubRun, repo, owner, name string, windowStart, windowEnd time.Time) ([]EvidenceItem, FamilyReport) {
	query := url.Values{}
	path := "/repos/" + owner + "/" + name + "/events"
	var items []EvidenceItem
	decode := func(body []byte) (int, error) {
		var docs []ghEventDoc
		if err := json.Unmarshal(body, &docs); err != nil {
			return 0, err
		}
		for _, doc := range docs {
			at, err := time.Parse(time.RFC3339, doc.CreatedAt)
			if err != nil {
				continue
			}
			if at.Before(windowStart) {
				continue
			}
			if !at.Before(windowEnd) {
				continue
			}
			if doc.Actor == nil || doc.Actor.Login != g.scope.user {
				continue
			}
			ev := evidenceFromEvent(doc, repo, at, g.scope.user, windowStart, windowEnd)
			if ev != nil {
				items = append(items, *ev)
			}
		}
		return len(docs), nil
	}
	walk := g.familyPageLoop(query, path, maxGithubFamilyPages, decode)
	n := len(items)
	if walk.err != nil {
		if walk.stop == "http" && walk.httpStatus == http.StatusUnprocessableEntity {
			detail := niceJoin(sanitizeHTTPDiagnostic(walk.httpStatus, walk.httpBody), fmt.Sprintf(
				"%d attributable event(s) retained before the provider rejected pagination; repository-event coverage remains incomplete because exhaustion of the retained window was not established", n))
			return items, FamilyReport{Family: "repo_events", QueriedRepo: repo, Count: n, Status: StatusIncomplete, Detail: detail}
		}
		status, detail := familyStatus(walk, n, "repository events")
		return items, FamilyReport{Family: "repo_events", QueriedRepo: repo, Count: n, Status: status, Detail: detail}
	}
	if walk.stop == "capped" {
		status, detail := familyStatus(walk, n, "repository events")
		return items, FamilyReport{Family: "repo_events", QueriedRepo: repo, Count: n, Status: status, Detail: detail}
	}
	_, detail := familyStatus(walk, n, "repository events")
	detail = niceJoin(detail, "the events timeline is bounded by the provider (only the past 30 days, up to 300 events per timeline); exhausting its retained pages establishes windowed coverage, not completeness for the selected day")
	return items, FamilyReport{Family: "repo_events", QueriedRepo: repo, Count: n, Status: StatusWindowed, Detail: detail}
}

func evidenceFromEvent(doc ghEventDoc, repo string, at time.Time, user string, windowStart, windowEnd time.Time) *EvidenceItem {
	if doc.Actor == nil {
		return nil
	}
	actor := doc.Actor.Login
	base := EvidenceItem{
		Actor:      actor,
		Time:       at.UTC().Format(time.RFC3339),
		Repo:       repo,
		Provenance: "repos/events",
	}
	var mergeAt time.Time
	windowedMerge := false
	if strings.EqualFold(doc.Type, "PullRequestEvent") && doc.Payload.Action == "closed" &&
		doc.Payload.PullRequest != nil && doc.Payload.PullRequest.Merged &&
		doc.Payload.PullRequest.MergedBy != nil && doc.Payload.PullRequest.MergedBy.Login == user &&
		doc.Payload.PullRequest.Number != 0 {
		if parsed, err := time.Parse(time.RFC3339, doc.Payload.PullRequest.MergedAt); err == nil &&
			!parsed.Before(windowStart) && parsed.Before(windowEnd) {
			mergeAt, windowedMerge = parsed, true
		}
	}
	switch {
	case windowedMerge:
		base.Ref = fmt.Sprintf("gh:%s:pr-merge:%d", repo, doc.Payload.PullRequest.Number)
		base.Kind = "merged_pull_request"
		base.Resource = fmt.Sprintf("pr:%s:%d", repo, doc.Payload.PullRequest.Number)
		base.URL = doc.Payload.PullRequest.HTMLURL
		base.Title = sanitizerText(doc.Payload.PullRequest.Title)
		base.Time = mergeAt.UTC().Format(time.RFC3339)
		return &base
	case strings.EqualFold(doc.Type, "DiscussionEvent") && doc.Payload.Discussion != nil && doc.Payload.Discussion.Number != 0:
		base.Ref = fmt.Sprintf("gh:%s:discussion:%d", repo, doc.Payload.Discussion.Number)
		base.Kind = "discussion"
		base.Resource = fmt.Sprintf("discussion:%s:%d", repo, doc.Payload.Discussion.Number)
		base.Title = sanitizerText(doc.Payload.Discussion.Title)
		return &base
	case strings.EqualFold(doc.Type, "DiscussionCommentEvent") && doc.Payload.Discussion != nil &&
		doc.Payload.Discussion.Number != 0 && doc.Payload.Comment != nil:
		base.Ref = fmt.Sprintf("gh:%s:discussion-comment:%d", repo, doc.Payload.Comment.ID)
		base.Kind = "discussion_comment"
		base.Resource = fmt.Sprintf("discussion:%s:%d", repo, doc.Payload.Discussion.Number)
		base.URL = doc.Payload.Comment.HTMLURL
		base.Title = sanitizerText(fmt.Sprintf("discussion comment on %s#%d", repo, doc.Payload.Discussion.Number))
		return &base
	case strings.EqualFold(doc.Type, "PullRequestReviewEvent") && doc.Payload.PullRequest != nil && doc.Payload.Review != nil:
		base.Ref = fmt.Sprintf("gh:%s:review:%d", repo, doc.Payload.Review.ID)
		base.Kind = "submitted_pr_review"
		base.Resource = fmt.Sprintf("pr:%s:%d", repo, doc.Payload.PullRequest.Number)
		base.URL = doc.Payload.Review.HTMLURL
		base.Title = sanitizerText(fmt.Sprintf("review on %s#%d (%s)", repo, doc.Payload.PullRequest.Number, doc.Payload.PullRequest.Title))
		return &base
	case strings.EqualFold(doc.Type, "PullRequestReviewCommentEvent") && doc.Payload.PullRequest != nil && doc.Payload.Comment != nil:
		base.Ref = fmt.Sprintf("gh:%s:pr-review-comment:%d", repo, doc.Payload.Comment.ID)
		base.Kind = "pr_review_comment"
		base.Resource = fmt.Sprintf("pr:%s:%d", repo, doc.Payload.PullRequest.Number)
		base.URL = doc.Payload.Comment.HTMLURL
		base.Title = sanitizerText(fmt.Sprintf("review comment on %s#%d (%s)", repo, doc.Payload.PullRequest.Number, doc.Payload.PullRequest.Title))
		return &base
	case strings.EqualFold(doc.Type, "IssueCommentEvent") && doc.Payload.Issue != nil && doc.Payload.Comment != nil:
		base.Ref = fmt.Sprintf("gh:%s:issue-comment:%d", repo, doc.Payload.Comment.ID)
		base.Kind = "issue_comment"
		base.Resource = fmt.Sprintf("issue:%s:%d", repo, doc.Payload.Issue.Number)
		base.URL = doc.Payload.Comment.HTMLURL
		base.Title = sanitizerText(fmt.Sprintf("issue comment on %s#%d", repo, doc.Payload.Issue.Number))
		return &base
	default:
		if doc.ID == "" {
			return nil
		}
		base.Ref = "gh:" + repo + ":event:" + doc.ID
		base.Kind = "event"
		base.Title = providerEventTypeName(doc.Type)
		return &base
	}
}

var providerURLRE = regexp.MustCompile(`https?://\S+`)

// sanitizeHTTPDiagnostic renders a bounded, sanitized provider-error
// diagnostic for a family Detail: the HTTP status plus a bounded snippet of
// the provider body. Provider bodies embed documentation links, so URLs are
// stripped and control characters are replaced; credential-shaped text outside
// a URL is NOT redacted here, so a caller must not treat the detail as
// credential-free.
func sanitizeHTTPDiagnostic(status int, body []byte) string {
	snippet := strings.TrimSpace(providerURLRE.ReplaceAllString(sanitizerText(string(body)), ""))
	snippet = strings.TrimSpace(strings.Trim(snippet, "\",:' "))
	if snippet == "" {
		return fmt.Sprintf("the provider responded with HTTP %d; no readable provider error body", status)
	}
	return fmt.Sprintf("the provider responded with HTTP %d; provider error body: %s", status, snippet)
}

func familyStatus(walk familyWalk, n int, label string) (string, string) {
	if walk.err != nil {
		return StatusFailed, truncateRunes(walk.err.Error(), maxDetailRunes)
	}
	switch walk.stop {
	case "finished", "empty":
		if n == 0 {
			return "empty", fmt.Sprintf("no attributable %s items in the day's window (pagination exhausted after %d page(s))", label, walk.pages+1)
		}
		return StatusComplete, fmt.Sprintf("%d item(s) retained; pagination exhausted after %d page(s)", n, walk.pages+1)
	case stopWindowPassed:
		proof := fmt.Sprintf("window end reached on page %d under the documented created-ascending sort", walk.pages+1)
		if n == 0 {
			return "empty", fmt.Sprintf("no attributable %s items in the day's window (%s)", label, proof)
		}
		return StatusComplete, fmt.Sprintf("%d item(s) retained; walk covered %s .. %s (%s)",
			n, walk.coveredFrom.UTC().Format(time.RFC3339), walk.coveredUntil.UTC().Format(time.RFC3339), proof)
	case "capped":
		return StatusIncomplete, fmt.Sprintf(
			"the collector's %d-page pagination ceiling was reached; coverage not established beyond the %d page(s) examined for %s",
			walk.maxPages, walk.maxPages, label)
	default:
		return StatusIncomplete, fmt.Sprintf("%s coverage is incomplete (%s)", label, walk.stop)
	}
}

func niceJoin(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// sanitizerText strips control characters and truncates provider text. It
// never inspects credentials (none are in provider payloads).
func sanitizerText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return truncateRunes(strings.TrimSpace(s), maxTitleRunes)
}

func pullRefFromURL(u, repo string) string {
	if m := pullNumberTailRE.FindStringSubmatch(u); m != nil {
		return fmt.Sprintf("%s#%s", repo, m[1])
	}
	return "pull request in " + repo
}

func issueRefFromURL(u, repo string) string {
	if m := issueNumberTailRE.FindStringSubmatch(u); m != nil {
		return fmt.Sprintf("%s#%s", repo, m[1])
	}
	return "issue in " + repo
}

func pullResourceFromURL(u, repo string) string {
	if m := pullNumberTailRE.FindStringSubmatch(u); m != nil {
		return fmt.Sprintf("pr:%s:%s", repo, m[1])
	}
	return ""
}

func issueResourceFromURL(u, repo string) string {
	if m := issueNumberTailRE.FindStringSubmatch(u); m != nil {
		return fmt.Sprintf("issue:%s:%s", repo, m[1])
	}
	return ""
}

func providerEventTypeName(t string) string {
	t = strings.TrimSpace(t)
	switch t {
	case "":
		return "provider event"
	}
	return strings.ToLower(strings.TrimSuffix(t, "Event"))
}

func buildGithubTopics(items []EvidenceItem) []TopicCandidate {
	byRef := map[string]int{}
	var deduped []EvidenceItem
	for _, item := range items {
		if idx, ok := byRef[item.Ref]; ok {
			if !strings.Contains(","+deduped[idx].Provenance+",", ","+item.Provenance+",") {
				deduped[idx].Provenance = deduped[idx].Provenance + "," + item.Provenance
			}
			continue
		}
		byRef[item.Ref] = len(deduped)
		deduped = append(deduped, item)
	}

	byResource := map[string]*TopicCandidate{}
	type entry struct {
		key string
		t   *TopicCandidate
	}
	var order []entry

	totalOverflow := 0
	for _, item := range deduped {
		key, title := githubTopicKey(item)
		t, ok := byResource[key]
		if !ok {
			t = &TopicCandidate{Ref: "tc:" + key, Title: title, Provider: "github"}
			byResource[key] = t
			order = append(order, entry{key: key, t: t})
		}
		switch {
		case len(t.Items) < maxTopicItems:
			t.Items = append(t.Items, item)
		case len(t.OverflowItems) < maxTopicOverflowItems && totalOverflow < maxTotalOverflowItems:
			t.OverflowItems = append(t.OverflowItems, item)
			totalOverflow++
		default:
			t.DroppedItems++
		}
	}

	var out []TopicCandidate
	for _, e := range order {
		t := e.t
		if t.DroppedItems > 0 || len(t.OverflowItems) > 0 {
			t.Truncated = true
		}
		sort.SliceStable(t.Items, func(x, y int) bool {
			if t.Items[x].Time != t.Items[y].Time {
				return t.Items[x].Time < t.Items[y].Time
			}
			return t.Items[x].Ref < t.Items[y].Ref
		})
		sort.SliceStable(t.OverflowItems, func(x, y int) bool {
			if t.OverflowItems[x].Time != t.OverflowItems[y].Time {
				return t.OverflowItems[x].Time < t.OverflowItems[y].Time
			}
			return t.OverflowItems[x].Ref < t.OverflowItems[y].Ref
		})
		if len(t.Items) > 0 {
			t.FirstAt = t.Items[0].Time
			t.LastAt = t.Items[len(t.Items)-1].Time
		}
		out = append(out, *t)
	}
	return out
}

func githubTopicKey(item EvidenceItem) (string, string) {
	repo := item.Repo
	if repo == "" {
		repo = "unknown-repository"
	}
	if kind, groupRepo, number, ok := resourceGroupID(item.Resource); ok {
		switch kind {
		case "pr":
			return "gh:" + groupRepo + "#pr:" + number, fmt.Sprintf("Pull request #%s (%s)", number, groupRepo)
		case "issue":
			return "gh:" + groupRepo + "#issue:" + number, fmt.Sprintf("Issue #%s activity (%s)", number, groupRepo)
		case "discussion":
			return "gh:" + groupRepo + "#discussion:" + number, fmt.Sprintf("Discussion #%s (%s)", number, groupRepo)
		}
	}
	if item.Kind == "commit" {
		return "gh:" + repo + ":commits", "Commits (" + repo + ")"
	}
	return "gh:" + repo + ":other-activity", "Other attributable activity (" + repo + ")"
}

func resourceGroupID(resource string) (kind, groupRepo, number string, ok bool) {
	parts := strings.SplitN(resource, ":", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	switch parts[0] {
	case "pr", "issue", "discussion":
		if parts[1] == "" || parts[2] == "" {
			return "", "", "", false
		}
		return parts[0], parts[1], parts[2], true
	}
	return "", "", "", false
}

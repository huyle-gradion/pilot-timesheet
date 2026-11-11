package timesheet

// GradionReader verifies READ capabilities only: it requires timesheet__get_my_week and
// timesheet__get_classifications and has no method that can dispatch log_time,
// edit_time, a delete tool, a REST call, or an arbitrary tool name. It exposes no
// generic MCP call operation. The endpoint is operator-configured and the token
// is resolved by the compiled binary through the documented dotenv/inherited
// precedence; neither value is ever echoed into a response, receipt, error or
// state document.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/mcp"
)

const (
	toolGetMyWeek          = "timesheet__get_my_week"
	toolGetClassifications = "timesheet__get_classifications"
)

const (
	gradionTimeout          = 15 * time.Second
	maxGradionResponseBytes = 8 << 20
)

type GradionReader struct {
	session  *mcp.Session
	verified bool
}

func missingCredentialError(name string) error {
	return newError(CodeRemoteUnavailable,
		"credential "+name+" is missing; export it in the process environment or pass --env-file with a file that defines it")
}

// NewGradionReader resolves the configured credential NAME and builds a session
// over the operator-configured endpoint. It performs no IO.
func NewGradionReader(cfg *Config, env map[string]string) (*GradionReader, error) {
	if cfg == nil || cfg.Gradion == nil || !cfg.Gradion.Enabled {
		return nil, newError(CodeRemoteDisabled, "read-only remote access is not enabled in configuration")
	}
	name := cfg.Gradion.TokenVariable()
	token, ok := resolveEnv(env, name)
	if !ok || strings.TrimSpace(token) == "" {
		return nil, missingCredentialError(name)
	}
	session, err := mcp.NewSession(mcp.Config{
		Endpoint:         cfg.Gradion.Endpoint,
		Token:            token,
		Timeout:          gradionTimeout,
		MaxResponseBytes: maxGradionResponseBytes,
		ClientName:       "timesheet",
		ClientVersion:    AppVersion,
	})
	if err != nil {
		return nil, newError(CodeRemoteUnavailable, "read-only remote session cannot be created")
	}
	return &GradionReader{session: session}, nil
}

// Close releases the session; it performs no IO.
func (r *GradionReader) Close() error {
	if r == nil || r.session == nil {
		return nil
	}
	return r.session.Close()
}

func (r *GradionReader) verifyReadCapabilities(ctx context.Context) error {
	if r.verified {
		return nil
	}
	if _, err := r.session.Initialize(ctx); err != nil {
		return remoteError(err)
	}
	tools, err := r.session.ListTools(ctx)
	if err != nil {
		return remoteError(err)
	}
	have := make(map[string]bool, len(tools))
	for _, t := range tools {
		have[t.Name] = true
	}
	for _, required := range []string{toolGetMyWeek, toolGetClassifications} {
		if !have[required] {
			return newError(CodeRemoteUnavailable, "required read tool unavailable: "+required)
		}
	}
	r.verified = true
	return nil
}

func remoteError(err error) error {
	var mcpErr *mcp.Error
	if errors.As(err, &mcpErr) && mcpErr.StableCode != "" {
		return newError(CodeRemoteUnreadable, "the read-only remote endpoint could not be read ("+mcpErr.StableCode+")")
	}
	return newError(CodeRemoteUnreadable, "the read-only remote endpoint could not be read")
}

type remoteWeek struct {
	Days      []remoteWeekDay `json:"days"`
	WeekStart string          `json:"weekStart"`
	MinDate   string          `json:"minDate"`
	MaxDate   string          `json:"maxDate"`
	Today     string          `json:"today"`
}

type remoteWeekDay struct {
	Date    string            `json:"date"`
	Entries []remoteWeekEntry `json:"entries"`
}

type remoteWeekEntry struct {
	ID               string `json:"id"`
	StartTime        string `json:"startTime"`
	EndTime          string `json:"endTime"`
	ClassificationID string `json:"classificationId"`
	Description      string `json:"description"`
	DurationMinutes  *int   `json:"durationMinutes"`
	Status           string `json:"status"`
	Task             string `json:"task"`
}

func parseWeekPayload(res mcp.CallResult) (remoteWeek, error) {
	payload := bytes.TrimSpace(res.Structured)
	if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
		return remoteWeek{}, newError(CodeRemoteUnreadable, "the week read returned no structured content; a reference envelope is not an empty timesheet")
	}
	if payload[0] != '{' {
		return remoteWeek{}, newError(CodeRemoteUnreadable, "the week read was not a JSON object")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return remoteWeek{}, newError(CodeRemoteUnreadable, "the week read was not a JSON object")
	}
	rawDays, ok := obj["days"]
	if !ok {
		return remoteWeek{}, newError(CodeRemoteUnreadable, "the week read had no days list")
	}
	if err := validateWeekDayContainers(rawDays); err != nil {
		return remoteWeek{}, err
	}
	var week remoteWeek
	if err := json.Unmarshal(payload, &week); err != nil {
		return remoteWeek{}, newError(CodeRemoteUnreadable, "the week read had an unexpected shape")
	}
	return week, nil
}

func validateWeekDayContainers(rawDays json.RawMessage) error {
	trimmed := bytes.TrimSpace(rawDays)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return newError(CodeRemoteUnreadable, "the week read's days value was not a list")
	}
	var days []json.RawMessage
	if err := json.Unmarshal(trimmed, &days); err != nil {
		return newError(CodeRemoteUnreadable, "the week read's days value was not a list")
	}
	for _, rawDay := range days {
		day := bytes.TrimSpace(rawDay)
		if len(day) == 0 || day[0] != '{' {
			return newError(CodeRemoteUnreadable, "the week read contained a day that was not an object")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(day, &fields); err != nil {
			return newError(CodeRemoteUnreadable, "the week read contained a day that was not an object")
		}
		rawDate, ok := fields["date"]
		if !ok {
			return newError(CodeRemoteUnreadable, "the week read contained a day without a date")
		}
		var date string
		if err := json.Unmarshal(rawDate, &date); err != nil {
			return newError(CodeRemoteUnreadable, "the week read contained a day whose date was not a string")
		}
		rawEntries, ok := fields["entries"]
		if !ok {
			return newError(CodeRemoteUnreadable, "the week read contained a day without the required entries list")
		}
		entries := bytes.TrimSpace(rawEntries)
		if len(entries) == 0 || entries[0] != '[' {
			return newError(CodeRemoteUnreadable, "the week read contained a day whose entries value was not a list")
		}
	}
	return nil
}

// ObserveDate performs one read of the week containing date and returns a durable
// observation record. Coverage is explicit: a missing requested day, a malformed
// entry, a duplicate/conflicting id or a failed read makes Complete false, and a
// failed read replaces any previous complete observation so it can never stand in
// as current proof of absence.
func (r *GradionReader) ObserveDate(ctx context.Context, date string) *RemoteObservation {
	if err := r.verifyReadCapabilities(ctx); err != nil {
		obs := &RemoteObservation{Date: date, ObservedAt: time.Now().UTC().Format(time.RFC3339)}
		obs.Complete = false
		obs.Failed = true
		obs.Detail = err.Error()
		if app, ok := err.(*AppError); ok {
			obs.Detail = app.Code + ": " + app.Message
		}
		obs.ObservationID = observationID(date, nil, false, obs.Detail)
		return obs
	}
	res, err := r.session.CallTool(ctx, toolGetMyWeek, map[string]any{"weekStart": date})
	return dateObservationFromWeekCall(date, res, err)
}

func dateObservationFromWeekCall(date string, res mcp.CallResult, callErr error) *RemoteObservation {
	obs := &RemoteObservation{Date: date, ObservedAt: time.Now().UTC().Format(time.RFC3339)}
	fail := func(detail string) *RemoteObservation {
		obs.Complete = false
		obs.Failed = true
		obs.Detail = truncateRunes(detail, maxDetailRunes)
		obs.ObservationID = observationID(date, nil, false, detail)
		return obs
	}
	if callErr != nil {
		return fail("the week read failed (" + errorCode(remoteError(callErr)) + ")")
	}
	if res.IsError {
		return fail("the week read returned a tool-level error")
	}
	week, err := parseWeekPayload(res)
	if err != nil {
		return fail(err.Error())
	}

	var gaps []string
	covered := false
	for _, day := range week.Days {
		if day.Date == date {
			covered = true
			break
		}
	}
	if !covered {
		gaps = append(gaps, "the requested date was not present in the returned week")
	}
	if week.MinDate != "" && date < week.MinDate {
		gaps = append(gaps, "the requested date is before the provider's minimum covered date")
	}
	if week.MaxDate != "" && date > week.MaxDate {
		gaps = append(gaps, "the requested date is after the provider's maximum covered date")
	}

	seen := map[string]bool{}
	for _, day := range week.Days {
		if _, err := parseDate(day.Date); err != nil {
			gaps = append(gaps, "the response contained a day with an invalid date")
			continue
		}
		if day.Date != date {
			continue
		}
		for _, e := range day.Entries {
			id := strings.TrimSpace(e.ID)
			if id == "" {
				gaps = append(gaps, "an observed entry had no stable remote id")
				continue
			}
			if seen[id] {
				obs.Duplicates++
				gaps = append(gaps, "the response repeated the same remote id")
				continue
			}
			if _, err := parseClock(strings.TrimSpace(e.StartTime)); err != nil {
				gaps = append(gaps, "an observed entry had an invalid start time")
				continue
			}
			if _, err := parseClock(strings.TrimSpace(e.EndTime)); err != nil {
				gaps = append(gaps, "an observed entry had an invalid end time")
				continue
			}
			seen[id] = true
			obs.Entries = append(obs.Entries, RemoteEntry{
				RemoteID:         id,
				Date:             day.Date,
				StartTime:        strings.TrimSpace(e.StartTime),
				EndTime:          strings.TrimSpace(e.EndTime),
				ClassificationID: strings.TrimSpace(e.ClassificationID),
				Description:      e.Description,
				Task:             strings.TrimSpace(e.Task),
				Status:           strings.TrimSpace(e.Status),
				DurationMinutes:  cloneInt(e.DurationMinutes),
				ContentObserved:  true,
			})
		}
	}
	sort.SliceStable(obs.Entries, func(i, j int) bool { return obs.Entries[i].RemoteID < obs.Entries[j].RemoteID })
	if len(obs.Entries) > maxRemoteEntries {
		obs.Entries = obs.Entries[:maxRemoteEntries]
		gaps = append(gaps, "the observation exceeded the bounded entry count")
	}
	obs.Coverage = dedupeStrings(gaps)
	obs.Complete = len(obs.Coverage) == 0
	obs.Detail = truncateRunes(strings.Join(obs.Coverage, "; "), maxDetailRunes)
	obs.ObservationID = observationID(date, obs.Entries, obs.Complete, strings.Join(obs.Coverage, ";"))
	return obs
}

func observationID(date string, entries []RemoteEntry, complete bool, detail string) string {
	payload := struct {
		Date     string        `json:"date"`
		Complete bool          `json:"complete"`
		Detail   string        `json:"detail,omitempty"`
		Entries  []RemoteEntry `json:"entries,omitempty"`
	}{Date: date, Complete: complete, Detail: detail, Entries: entries}
	data, err := json.Marshal(payload)
	if err != nil {
		return sha256Hex([]byte(date))
	}
	return sha256Hex(data)
}

type ClassificationsReceipt struct {
	CatalogID  string                 `json:"catalog_id"`
	Query      string                 `json:"query,omitempty"`
	Complete   bool                   `json:"complete"`
	Options    []ClassificationOption `json:"options"`
	ObservedAt string                 `json:"observed_at"`
	Note       string                 `json:"note,omitempty"`
}

var (
	classificationEntryRE = regexp.MustCompile(`^-\s+(.+?)\s+\(([^)]*)\)\s+\[([^\]]*)\]\s*·\s*(\S+)\s*$`)
	classificationTruncRE = regexp.MustCompile(`(?i)^\s*\d+\+.*showing\s+\d+`)
	classificationHeadRE  = regexp.MustCompile(`(?i)^\s*\d+\s+classifications?\s+found`)
	// classificationEmptyRE recognizes the provider's zero-match rendering
	// (observed live as "No active classifications found."). An empty result
	// is a valid catalog, not an unreadable one.
	classificationEmptyRE = regexp.MustCompile(`(?i)^\s*(?:no|0)\s+(?:active\s+)?classifications?\s+(?:found|matched|match|available)\.?\s*$`)
)

// Classifications performs one bounded classification read. A filtered or
// truncated catalog can only show that an option was not returned; it never
// proves an option absent.
func (r *GradionReader) Classifications(ctx context.Context, query string) (*CatalogRecord, error) {
	if err := r.verifyReadCapabilities(ctx); err != nil {
		return nil, err
	}
	return classificationsCall(ctx, r.session, query)
}

func classificationsCall(ctx context.Context, session *mcp.Session, query string) (*CatalogRecord, error) {
	q := strings.TrimSpace(query)
	args := map[string]any{}
	if q != "" {
		args["search"] = q
	}
	res, err := session.CallTool(ctx, toolGetClassifications, args)
	if err != nil {
		return nil, remoteError(err)
	}
	if res.IsError {
		return nil, newError(CodeRemoteUnreadable, "the classification read returned a tool-level error")
	}
	cat, err := parseCatalog(res, q, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return cat, nil
}

type catalogReference struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

func parseCatalog(res mcp.CallResult, query string, observed time.Time) (*CatalogRecord, error) {
	cat := &CatalogRecord{Query: query, Complete: true, ObservedAt: observed.Format(time.RFC3339)}

	nameByID := map[string]string{}
	var structuredOrder []string
	trimmed := bytes.TrimSpace(res.Structured)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if trimmed[0] != '{' {
			return nil, newError(CodeRemoteUnreadable, "the classification read was not a JSON object")
		}
		var env struct {
			Resources []catalogReference `json:"resources"`
		}
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return nil, newError(CodeRemoteUnreadable, "the classification read had an unexpected shape")
		}
		for _, item := range env.Resources {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				continue
			}
			if _, ok := nameByID[id]; !ok {
				structuredOrder = append(structuredOrder, id)
			}
			nameByID[id] = strings.TrimSpace(item.Label)
		}
	}

	type textEntry struct{ Name, Code, Activity string }
	textByID := map[string]textEntry{}
	var textOrder []string
	text := strings.TrimSpace(res.Content)
	if text != "" {
		lines := strings.Split(text, "\n")
		parsed := 0
		header := false
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if classificationTruncRE.MatchString(line) {
				cat.Complete = false
				continue
			}
			if classificationHeadRE.MatchString(line) {
				header = true
				continue
			}
			m := classificationEntryRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			parsed++
			id := strings.TrimSpace(m[4])
			if _, ok := textByID[id]; !ok {
				textOrder = append(textOrder, id)
			}
			textByID[id] = textEntry{Name: strings.TrimSpace(m[1]), Code: strings.TrimSpace(m[2]), Activity: strings.TrimSpace(m[3])}
		}
		if parsed == 0 && len(structuredOrder) == 0 && !header {
			if classificationEmptyRE.MatchString(text) {
				cat.CatalogID = catalogID(query, cat.Options, cat.Complete)
				return cat, nil
			}
			return nil, newError(CodeRemoteUnreadable, "the classification text could not be parsed")
		}
		if len(structuredOrder) > 0 && parsed == 0 {
			cat.Complete = false
		}
	} else if len(structuredOrder) > 0 {
		cat.Complete = false
	}

	order := append([]string(nil), textOrder...)
	inText := map[string]bool{}
	for _, id := range textOrder {
		inText[id] = true
	}
	for _, id := range structuredOrder {
		if !inText[id] {
			order = append(order, id)
		}
	}
	for _, id := range order {
		name := nameByID[id]
		if name == "" {
			name = textByID[id].Name
		}
		cat.Options = append(cat.Options, ClassificationOption{
			ID: id, Name: name, Code: textByID[id].Code, ActivityType: textByID[id].Activity,
		})
	}
	if len(cat.Options) >= maxCatalogOptions {
		cat.Complete = false
	}
	cat.CatalogID = catalogID(query, cat.Options, cat.Complete)
	return cat, nil
}

func catalogID(query string, options []ClassificationOption, complete bool) string {
	payload := struct {
		Query    string                 `json:"query,omitempty"`
		Complete bool                   `json:"complete"`
		Options  []ClassificationOption `json:"options"`
	}{Query: strings.TrimSpace(query), Complete: complete, Options: options}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func contextWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), gradionTimeout)
}

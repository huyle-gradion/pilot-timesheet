// Package timesheet implements the local, review-first timesheet workflow for the
// timesheet line. R1 covers manual intervals, review, confirmation, local save,
// revision, cancellation, status, and restart-safe state. R2 adds the binary's
// own GitHub and calendar evidence collection for an opened day: strict
// protocol operations, hermetic-covered GitHub activity families, provenance-
// preserving topic candidates and proposed meetings, honest per-source
// completeness, and persisted human-only acknowledgment/attendance decisions.
// It still has no migration and no remote submission.
package timesheet

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Application identity.
//
// stateSchema is the schema this build writes. It still reads the accepted
// state schemas 1 and 2: each legacy file loads, is validated in its own
// shape, is upgraded in memory through an explicit conversion, and the on-disk
// file is only rewritten as schema 3 by a successful durable transaction, never
// by startup. Older loaders fail closed on schema 3 with state_invalid, which
// is intentional: they have no semantics for native approvals, write intents or
// execution history.
const (
	AppVersion        = "0.2.0"
	stateSchema       = 3
	legacyStateSchema = 1
	schemaTwoState    = 2

	maxDescriptionRunes = 500
	maxEvidenceRefs     = 20
	maxEvidenceRefRunes = 256
)

// Stable protocol error codes. They are part of the public interface and are
// documented in docs/agents/timesheet-interface.md.
const (
	CodeBadRequest       = "bad_request"
	CodeUnknownOp        = "unknown_op"
	CodeInvalidDate      = "invalid_date"
	CodeInvalidGrammar   = "invalid_grammar"
	CodeInvalidInterval  = "invalid_interval"
	CodeNonexistentTime  = "nonexistent_time"
	CodeAmbiguousTime    = "ambiguous_time"
	CodeOverlap          = "overlap"
	CodeUnknownProject   = "unknown_project"
	CodeUnknownDay       = "unknown_day"
	CodeUnknownInterval  = "unknown_interval"
	CodeStaleRevision    = "stale_revision"
	CodeNothingToConfirm = "nothing_to_confirm"
	CodeStoreLocked      = "store_locked"
	CodeStateInvalid     = "state_invalid"
	CodeWriteFailed      = "write_failed"
	CodeWriteUncertain   = "write_uncertain"
	CodeConfigInvalid    = "config_invalid"
	CodeDotenvInvalid    = "dotenv_invalid"
	CodeSourceGate       = "source_unacknowledged"
	CodeUnknownMeeting   = "unknown_meeting"
	CodeStaleCollection  = "stale_collection"

	// R3 migration codes. Source-level problems are reported as bounded
	// conflicts inside a preview; these codes cover request-level rejection.
	CodeMigrationDisabled = "migration_disabled"
	CodeMigrationInvalid  = "migration_invalid"
	CodeMigrationStale    = "migration_stale"
	CodeMigrationConflict = "migration_conflict"
	CodeSessionOpen       = "session_open"

	// R3 read-only remote boundary codes. They carry names and stable codes
	// only, never credential values, authenticated URLs or provider bodies.
	CodeRemoteDisabled    = "remote_disabled"
	CodeRemoteUnavailable = "remote_unavailable"
	CodeRemoteIncomplete  = "remote_incomplete"
	CodeRemoteUnreadable  = "remote_unreadable"
	CodeStaleObservation  = "stale_observation"
	CodeUnknownRemote     = "unknown_remote"
	CodeUnknownLocal      = "unknown_local"
	CodeIdentityConflict  = "identity_conflict"
	CodeUnknownClassify   = "unknown_classification"
	CodeStaleCatalog      = "stale_catalog"
	CodeNoConfirmedWork   = "no_confirmed_work"
	CodeDirtySession      = "dirty_session"

	// R4 native approval and verified-submission codes. They carry names and
	// stable codes only, never credential values, authenticated URLs or
	// provider bodies.
	CodeStalePlan                = "stale_plan"
	CodePlanNotReady             = "plan_not_ready"
	CodeUnknownApproval          = "unknown_approval"
	CodeStaleApproval            = "stale_approval"
	CodeClassificationUnverified = "classification_unverified"
	CodeClassificationChanged    = "classification_choice_changed"
	CodeSelectionUnverifiable    = "classification_choice_unverifiable"
	CodeUnknownWrite             = "unknown_write"

	CodeInternal = "internal"
)

// AppError is a protocol-safe error. Messages never contain credential values,
// raw dotenv lines, configuration dumps, or authenticated URLs.
type AppError struct {
	Code    string
	Message string
}

func (e *AppError) Error() string { return e.Code + ": " + e.Message }

func newError(code, message string) *AppError { return &AppError{Code: code, Message: message} }

func errorCode(err error) string {
	var app *AppError
	if errors.As(err, &app) {
		return app.Code
	}
	return CodeInternal
}

// Config is the non-secret operator-supplied configuration. RequiredEnv holds
// variable NAMES for later slices; local operations never require them.
// Sources describes the optional R2 evidence collectors; a nil Sources is the
// R1 manual-only configuration and must keep working unchanged.
type Config struct {
	Timezone    string         `json:"timezone"`
	StatePath   string         `json:"state_path"`
	Projects    []Project      `json:"projects"`
	RequiredEnv []string       `json:"required_env,omitempty"`
	Sources     *SourcesConfig `json:"sources,omitempty"`
	// Migration is the disabled-by-default explicit legacy import provision.
	// It names the authoritative legacy records root and the timezone that
	// governed those records; old records do not carry their own timezone.
	Migration *MigrationConfig `json:"migration,omitempty"`
	// Gradion is the disabled-by-default read-only remote boundary. It carries
	// an operator-configured endpoint and a credential variable NAME only.
	Gradion *GradionConfig `json:"gradion,omitempty"`
	// WorkHours is the optional main-work-time policy used only to annotate
	// the one-day draft. It never rejects, warns, or changes a recorded block.
	// The four-hour per-entry bound is the remote's fixed policy, not a
	// work_hours setting; unknown keys inside this block fail closed.
	WorkHours *WorkHoursConfig `json:"work_hours,omitempty"`
	// Judge is the optional binary-owned judge (ADR 0010); absent means off.
	Judge *JudgeConfig `json:"judge,omitempty"`
	// Daily is the optional scheduled-run policy (issue #243).
	Daily *DailyConfig `json:"daily,omitempty"`
}

// WorkHoursConfig names the weekdays and same-day windows of main work time.
// It is presentation-only policy: outside hours are allowed, with no ceiling.
// Days use exactly the lowercase three-letter tokens mon..sun; windows use
// 24-hour HH:MM and must not overlap. Configuration is validated offline so a
// draft annotation never has to guess.
type WorkHoursConfig struct {
	Days    []string     `json:"days"`
	Windows []WorkWindow `json:"windows"`
}

// WorkWindow is one inclusive-start, exclusive-end main work window. Start is
// before End on the same recorded day; a window never crosses midnight.
type WorkWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// MigrationConfig provisions explicit legacy migration. It is disabled unless
// Enabled is true; when enabled, LegacyRoot must name an existing records root
// containing the authoritative workflow/YYYY-MM-DD.json daily files and
// LegacyTimezone must name the IANA timezone those records were written in.
//
// ProjectMap and ClassificationMap are the only permitted translation of a
// legacy value the new configuration cannot represent directly. They are
// explicit provisions: R3 never invents a replacement, and every original value
// stays in audit.
type MigrationConfig struct {
	Enabled           bool              `json:"enabled"`
	LegacyRoot        string            `json:"legacy_root,omitempty"`
	LegacyTimezone    string            `json:"legacy_timezone,omitempty"`
	ProjectMap        map[string]string `json:"project_map,omitempty"`
	ClassificationMap map[string]string `json:"classification_map,omitempty"`
}

// DefaultGradionTokenEnv is the credential variable NAME the remote boundary
// reads when TokenEnv is not configured. Only the name is configuration; the
// value is resolved by the compiled binary from the documented dotenv/inherited
// environment precedence.
const DefaultGradionTokenEnv = "GRADION_MCP_TOKEN"

// GradionConfig is the non-secret read-only remote configuration. Endpoint is a
// operator-configured MCP endpoint; it is never echoed into a protocol
// response, error, receipt or state document.
type GradionConfig struct {
	DefaultClassification *ConfiguredDefaultClassification `json:"default_classification,omitempty"`
	Enabled               bool                             `json:"enabled"`
	Endpoint              string                           `json:"endpoint,omitempty"`
	TokenEnv              string                           `json:"token_env,omitempty"`
}

// TokenVariable returns the configured credential variable name.
func (g *GradionConfig) TokenVariable() string {
	if g == nil || strings.TrimSpace(g.TokenEnv) == "" {
		return DefaultGradionTokenEnv
	}
	return strings.TrimSpace(g.TokenEnv)
}

// SourcesConfig gates the two R2 evidence collectors. Enabled=false disables
// every collector and preserves the exact R1 manual-only workflow.
type SourcesConfig struct {
	Enabled  bool                  `json:"enabled"`
	Github   *GitHubSourceConfig   `json:"github,omitempty"`
	Calendar *CalendarSourceConfig `json:"calendar,omitempty"`
}

// GitHubSourceConfig names the configured identity and the explicit
// repositories whose attributable activity is collected. APIBase may point the
// collection HTTP calls at a trusted local service for verification; requests
// never accept an endpoint from the protocol layer.
type GitHubSourceConfig struct {
	Enabled      bool     `json:"enabled"`
	User         string   `json:"user"`
	Repositories []string `json:"repositories"`
	APIBase      string   `json:"api_base,omitempty"`
}

// CalendarSourceConfig is the private ICS feed source. The feed's URL is the
// credential GCAL_ICS_URL; it is never stored in configuration or state.
// User optionally names the calendar identity whose invitation answer is
// surfaced as the event's attendance status.
type CalendarSourceConfig struct {
	Enabled bool   `json:"enabled"`
	User    string `json:"user,omitempty"`
}

// EnabledSources returns which collectors will run, and whether any is on.
func (c *Config) EnabledSources() (github bool, calendar bool) {
	if c.Sources == nil || !c.Sources.Enabled {
		return false, false
	}
	if c.Sources.Github != nil && c.Sources.Github.Enabled {
		github = true
	}
	if c.Sources.Calendar != nil && c.Sources.Calendar.Enabled {
		calendar = true
	}
	return github, calendar
}

// Project is an explicit project/classification selection available to typed
// interval input. DefaultClassification is applied only when none is supplied.
type Project struct {
	ID                    string `json:"id"`
	Name                  string `json:"name,omitempty"`
	DefaultClassification string `json:"default_classification,omitempty"`
}

// State is the durable local store. It contains confirmed work and, since R2,
// per-date Evidence: collection families, topics, proposed meetings and
// explicit review decisions. Evidence is written atomically by the
// collect/acknowledge/decide operations; it is never part of confirmed work,
// revisions, or save receipts. A pre-R2 store without the field loads
// unchanged; however, a store file written by this R2 build cannot be read by
// a pre-R2 binary because its loader rejects the unknown field.
//
// Schema 2 (R3) adds Migration (explicit legacy import receipts and audit),
// Remote (stable bindings, read-only observations, resolutions and preserved
// write outcomes) and Plans (immutable previews). A schema-1 or schema-2 file
// is validated in its own shape, upgraded in memory through an explicit
// conversion, and only written as schema 3 by a successful durable
// transaction.
//
// Schema 3 (R4) adds Approvals (the native exact-plan approval ledger), Intents
// (durable pre-write intents with unique write references and attempt state)
// and Executions (native submission history). Plan records no longer carry
// persisted R3 authorization flags: the approval ledger is the only authority.
// A schema-3 file fails closed (state_invalid) in a schema-1/2-only loader,
// which is intentional: it has no semantics for native approvals, write
// intents or execution history.
type State struct {
	Schema     int                     `json:"schema"`
	SavedAt    string                  `json:"saved_at,omitempty"`
	Days       map[string]*Day         `json:"days"`
	Evidence   map[string]*DayEvidence `json:"evidence,omitempty"`
	Migration  *MigrationState         `json:"migration,omitempty"`
	Remote     *RemoteState            `json:"remote,omitempty"`
	Plans      map[string]*PlanRecord  `json:"plans,omitempty"`
	Approvals  []ApprovalRecord        `json:"approvals,omitempty"`
	Intents    []WriteIntent           `json:"intents,omitempty"`
	Executions []ExecutionRecord       `json:"executions,omitempty"`
}

// Day is the durable record for one calendrical day (the day an interval
// starts). Revision increments on confirmation; SavedRevision is tracked by the
// session to detect unsaved confirmed work.
type Day struct {
	Date         string         `json:"date"`
	Timezone     string         `json:"timezone"`
	Revision     int            `json:"revision"`
	NextInterval int            `json:"next_interval"`
	Confirmed    []Interval     `json:"confirmed"`
	History      []HistoryEntry `json:"history,omitempty"`

	// Session-only review state. Neither field is persisted or exposed in state.
	Staged        []Interval `json:"-"`
	SavedRevision int        `json:"-"`
}

// HistoryEntry records a confirmed revision for later slices. It contains the
// confirmed local snapshot, no remote submission state, and grants no
// authorization.
type HistoryEntry struct {
	Revision     int        `json:"revision"`
	ConfirmedAt  string     `json:"confirmed_at"`
	Intervals    int        `json:"intervals"`
	TotalMinutes int        `json:"total_minutes"`
	Snapshot     []Interval `json:"snapshot,omitempty"`
}

// Interval is a user-supplied manual time block. Date is the local date on
// which it starts. End earlier than Start (but not 00:00) means the interval
// crosses midnight; End 00:00 with a later Start ends exactly at midnight.
type Interval struct {
	ID             string   `json:"id"`
	Date           string   `json:"date"`
	Start          string   `json:"start"`
	End            string   `json:"end"`
	Project        string   `json:"project"`
	Classification string   `json:"classification,omitempty"`
	Description    string   `json:"description"`
	EvidenceRefs   []string `json:"evidence_refs,omitempty"`

	// RemoteClassificationID is the explicit reviewed Gradion catalog option
	// bound to this interval by select_classification. It is the ONLY source of
	// a previewed remote classification; a project default or a migrated hint
	// is never selected silently.
	RemoteClassificationID string `json:"remote_classification_id,omitempty"`
	// RemoteCatalogID is the catalog identity of that reviewed choice (audit).
	RemoteCatalogID string `json:"remote_catalog_id,omitempty"`
	// MigratedClassificationID preserves the legacy explicit classification
	// value verbatim as an UNVERIFIED hint. It never becomes a remote selection
	// by itself: it must be revalidated against a fresh catalog and chosen with
	// select_classification.
	MigratedClassificationID string `json:"migrated_classification_id,omitempty"`
}

// Allocation is the portion of an interval that falls on one local day.
type Allocation struct {
	Date    string `json:"date"`
	Start   string `json:"start"`
	End     string `json:"end"`
	Minutes int    `json:"minutes"`
}

// Segment is the rendered portion of one interval on one day. Locked means the
// segment spills in from an interval that started on an earlier day and must be
// edited there, not in the viewed day.
type Segment struct {
	IntervalID      string       `json:"interval_id"`
	Date            string       `json:"date"`
	Start           string       `json:"start"`
	End             string       `json:"end"`
	Minutes         int          `json:"minutes"`
	Project         string       `json:"project"`
	Classification  string       `json:"classification,omitempty"`
	Description     string       `json:"description"`
	EvidenceRefs    []string     `json:"evidence_refs,omitempty"`
	OriginDate      string       `json:"origin_date"`
	Locked          bool         `json:"locked"`
	CrossesMidnight bool         `json:"crosses_midnight"`
	Allocations     []Allocation `json:"allocations,omitempty"`
}

// View is a day's rendered segments and their accurate total.
type View struct {
	Segments     []Segment `json:"segments"`
	TotalMinutes int       `json:"total_minutes"`
}

var (
	intervalLineRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})[ \t]+(\d{2}):(\d{2})-(\d{2}):(\d{2})[ \t]+([^ \t]+)[ \t]+(.+)$`)
	projectIDRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	envNameRE      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	controlRune    = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

func parseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil || t.Format("2006-01-02") != s {
		return time.Time{}, newError(CodeInvalidDate, "date must be a real calendar date in YYYY-MM-DD form")
	}
	return t, nil
}

func nextDate(s string) string {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s
	}
	return t.AddDate(0, 0, 1).Format("2006-01-02")
}

func parseClock(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, newError(CodeInvalidInterval, "time must use 24-hour HH:MM form")
	}
	// Atoi accepts leading signs, so "+1:00" and "09:+5" would otherwise parse.
	// The grammar is digits only; nothing is normalised.
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, newError(CodeInvalidInterval, "time must use 24-hour HH:MM form")
		}
	}
	h, err1 := strconv.Atoi(s[0:2])
	m, err2 := strconv.Atoi(s[3:5])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, newError(CodeInvalidInterval, "time must use 24-hour HH:MM form")
	}
	return h*60 + m, nil
}

// ParseInterval parses the documented typed/manual grammar:
//
//	DATE HH:MM-HH:MM PROJECT[/CLASSIFICATION] DESCRIPTION
//
// expectedDate must match DATE. The endpoints are resolved in loc, the recorded
// day's timezone, so nonexistent or ambiguous local times fail closed. Multiple
// evidence references can be attached separately; they never add minutes.
func ParseInterval(text, expectedDate string, cfg *Config, loc *time.Location) (Interval, error) {
	text = strings.TrimSpace(text)
	m := intervalLineRE.FindStringSubmatch(text)
	if m == nil || strings.ContainsAny(text, "\r\n") {
		return Interval{}, newError(CodeInvalidGrammar, `interval must match "DATE HH:MM-HH:MM PROJECT[/CLASSIFICATION] DESCRIPTION"`)
	}
	date, start, end, projectToken, description := m[1], m[2]+":"+m[3], m[4]+":"+m[5], m[6], strings.TrimSpace(m[7])
	if date != expectedDate {
		return Interval{}, newError(CodeInvalidInterval, "interval date must match the open day")
	}
	if _, err := parseClock(start); err != nil {
		return Interval{}, err
	}
	if _, err := parseClock(end); err != nil {
		return Interval{}, err
	}
	if len(description) >= 2 {
		if (description[0] == '"' && description[len(description)-1] == '"') ||
			(description[0] == '\'' && description[len(description)-1] == '\'') {
			description = strings.TrimSpace(description[1 : len(description)-1])
		}
	}
	if description == "" {
		return Interval{}, newError(CodeInvalidInterval, "description must not be empty")
	}
	if len([]rune(description)) > maxDescriptionRunes {
		return Interval{}, newError(CodeInvalidInterval, "description is too long")
	}
	if controlRune.MatchString(description) {
		return Interval{}, newError(CodeInvalidInterval, "description must not contain control characters")
	}

	project, classification := projectToken, ""
	if i := strings.IndexByte(projectToken, '/'); i >= 0 {
		project, classification = projectToken[:i], projectToken[i+1:]
		if classification == "" {
			return Interval{}, newError(CodeInvalidInterval, "classification after '/' must not be empty")
		}
	}
	if project == "" || !projectIDRE.MatchString(project) {
		return Interval{}, newError(CodeInvalidInterval, "project is not a valid project identifier")
	}
	if classification != "" && !projectIDRE.MatchString(classification) {
		return Interval{}, newError(CodeInvalidInterval, "classification is not a valid classification identifier")
	}
	proj, ok := cfg.project(project)
	if !ok {
		return Interval{}, newError(CodeUnknownProject, "project is not configured")
	}
	if classification == "" {
		classification = proj.DefaultClassification
	}

	iv := Interval{
		Date:           date,
		Start:          start,
		End:            end,
		Project:        project,
		Classification: classification,
		Description:    description,
	}
	if _, _, err := iv.resolveEndpoints(loc); err != nil {
		return Interval{}, err
	}
	return iv, nil
}

func (c *Config) project(id string) (Project, bool) {
	for _, p := range c.Projects {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

// dayLocation returns the recorded day's timezone. It never substitutes the
// currently configured timezone, so saved work is not reinterpreted when the
// operator changes configuration.
func dayLocation(timezone string) (*time.Location, error) {
	if strings.TrimSpace(timezone) == "" {
		return nil, newError(CodeStateInvalid, "recorded day has no timezone")
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, newError(CodeStateInvalid, "recorded day timezone is not a recognised IANA timezone")
	}
	return loc, nil
}

// resolveLocal resolves one local wall-clock endpoint in loc. It returns
// nonexistent_time for skipped wall times and ambiguous_time when the wall time
// repeats. It never silently chooses an occurrence.
func resolveLocal(date, clock string, loc *time.Location, which string) (time.Time, error) {
	if loc == nil {
		return time.Time{}, newError(CodeStateInvalid, "recorded day timezone is unavailable")
	}
	d, err := parseDate(date)
	if err != nil {
		return time.Time{}, err
	}
	minute, err := parseClock(clock)
	if err != nil {
		return time.Time{}, err
	}
	y, month, day := d.Date()
	hour, mins := minute/60, minute%60
	wallUTC := time.Date(y, month, day, hour, mins, 0, 0, time.UTC)
	mid := time.Date(y, month, day, 12, 0, 0, 0, loc)

	offsets := map[int]bool{}
	for delta := -48 * time.Hour; delta <= 48*time.Hour; delta += 6 * time.Hour {
		_, offset := mid.Add(delta).Zone()
		offsets[offset] = true
	}
	var matches []time.Time
	seen := map[int64]bool{}
	for offset := range offsets {
		instant := wallUTC.Add(-time.Duration(offset) * time.Second)
		local := instant.In(loc)
		if local.Year() != y || local.Month() != month || local.Day() != day ||
			local.Hour() != hour || local.Minute() != mins {
			continue
		}
		if !seen[instant.UnixNano()] {
			seen[instant.UnixNano()] = true
			matches = append(matches, instant)
		}
	}
	switch len(matches) {
	case 0:
		return time.Time{}, newError(CodeNonexistentTime, fmt.Sprintf(
			"%s %s %s does not exist in the recorded day's timezone; choose a time outside the DST transition",
			which, date, clock))
	case 1:
		return matches[0], nil
	default:
		return time.Time{}, newError(CodeAmbiguousTime, fmt.Sprintf(
			"%s %s %s occurs twice in the recorded day's timezone; this build does not choose an occurrence; choose a time outside the repeated hour",
			which, date, clock))
	}
}

// resolveEndpoints turns the wall-clock interval into absolute endpoints in the
// recorded day's timezone. DATE is the start date; an end clock at or before the
// start clock belongs to the next local date (00:00 ends exactly at midnight).
func (iv Interval) resolveEndpoints(loc *time.Location) (time.Time, time.Time, error) {
	startMin, err := parseClock(iv.Start)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	endMin, err := parseClock(iv.End)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if startMin == endMin {
		return time.Time{}, time.Time{}, newError(CodeInvalidInterval, "interval duration must be positive")
	}
	endDate := iv.Date
	if endMin == 0 && startMin > 0 {
		endDate = nextDate(iv.Date)
	} else if endMin < startMin {
		endDate = nextDate(iv.Date)
	}
	start, err := resolveLocal(iv.Date, iv.Start, loc, "start")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := resolveLocal(endDate, iv.End, loc, "end")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, newError(CodeInvalidInterval, "interval duration must be positive")
	}
	return start, end, nil
}

// Duration returns elapsed minutes using absolute timezone-aware endpoints. A
// spring-forward interval is shorter than its wall-clock difference; a
// fall-back interval that spans the repeated hour is longer.
func (iv Interval) Duration(loc *time.Location) (int, error) {
	start, end, err := iv.resolveEndpoints(loc)
	if err != nil {
		return 0, err
	}
	minutes := int(end.Sub(start) / time.Minute)
	if minutes <= 0 {
		return 0, newError(CodeInvalidInterval, "interval duration must be positive")
	}
	return minutes, nil
}

// localDayStart returns the earliest instant of date in loc. It is used only for
// calendar-day allocation boundaries, not for interval endpoints. A date whose
// midnight is skipped by a transition starts at its first real instant; only a
// civil date with no real instants at all is reported as nonexistent.
func localDayStart(date string, loc *time.Location) (time.Time, error) {
	d, err := parseDate(date)
	if err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		return time.Time{}, newError(CodeStateInvalid, "recorded day timezone is unavailable")
	}
	y, month, day := d.Date()
	// A wholly absent civil date renders even its midday in a neighboring
	// date. Midday is the existence probe: a date can be missing it only if
	// no instant of the date exists at all. time.Date normalization is not
	// such a proof, because a skipped midnight normalizes the same way.
	mid := time.Date(y, month, day, 12, 0, 0, 0, loc)
	if mid.In(loc).Format("2006-01-02") != date {
		return time.Time{}, newError(CodeInvalidInterval, "local day does not exist in the recorded day's timezone")
	}
	t := time.Date(y, month, day, 0, 0, 0, 0, loc)
	// If the date exists but its midnight is skipped, walk forward to the
	// date's first real instant.
	for i := 0; i < 1440; i++ {
		if t.In(loc).Format("2006-01-02") == date {
			break
		}
		t = t.Add(time.Minute)
	}
	if t.In(loc).Format("2006-01-02") != date {
		return time.Time{}, newError(CodeInvalidInterval, "local day does not exist in the recorded day's timezone")
	}
	// If local midnight is repeated or normalization landed inside the date
	// after its first real instant, walk back to the earliest occurrence.
	for i := 0; i < 180; i++ {
		previous := t.Add(-time.Minute)
		if previous.In(loc).Format("2006-01-02") != date {
			break
		}
		t = previous
	}
	return t, nil
}

// ResolvedAllocations splits the interval at local day boundaries in loc and
// reports elapsed minutes per local date. Each boundary is the earliest real
// instant of the date, so a day whose midnight is skipped starts at its first
// instant and its allocation label shows that actual local start. The
// allocation dates and the totals use the recorded day's timezone, not the
// currently configured timezone.
func (iv Interval) ResolvedAllocations(loc *time.Location) ([]Allocation, error) {
	start, end, err := iv.resolveEndpoints(loc)
	if err != nil {
		return nil, err
	}
	startDate := start.In(loc).Format("2006-01-02")
	endDate := end.In(loc).Format("2006-01-02")
	var out []Allocation
	current := startDate
	for i := 0; i < 4; i++ {
		next := nextDate(current)
		dayStart, err := localDayStart(current, loc)
		if err != nil {
			return nil, err
		}
		dayEnd, err := localDayStart(next, loc)
		if err != nil {
			return nil, err
		}
		segmentStart, segmentEnd := start, end
		if dayStart.After(segmentStart) {
			segmentStart = dayStart
		}
		if dayEnd.Before(segmentEnd) {
			segmentEnd = dayEnd
		}
		if segmentEnd.After(segmentStart) {
			// The continuation start is the local wall time of the actual
			// segment start, not a hard-coded 00:00 that a skipped midnight
			// can make nonexistent. Whatever the boundary resolves to is what
			// the interval start renders as when the segment begins there.
			startText := segmentStart.In(loc).Format("15:04")
			endText := segmentEnd.In(loc).Format("15:04")
			if segmentEnd.Equal(dayEnd) {
				endText = "24:00"
			} else if segmentEnd.Equal(end) {
				endText = iv.End
			}
			minutes := int(segmentEnd.Sub(segmentStart) / time.Minute)
			if minutes <= 0 {
				return nil, newError(CodeInvalidInterval, "interval allocation is not positive")
			}
			out = append(out, Allocation{Date: current, Start: startText, End: endText, Minutes: minutes})
		}
		if current == endDate {
			break
		}
		current = next
	}
	if len(out) == 0 {
		return nil, newError(CodeInvalidInterval, "interval has no positive local allocation")
	}
	return out, nil
}

// CrossesMidnight reports whether the interval continues into the next day.
func (iv Interval) CrossesMidnight() bool {
	start, err1 := parseClock(iv.Start)
	end, err2 := parseClock(iv.End)
	if err1 != nil || err2 != nil {
		return false
	}
	return end > 0 && end < start
}

func (iv Interval) clone() Interval {
	out := iv
	out.EvidenceRefs = append([]string(nil), iv.EvidenceRefs...)
	return out
}

func cloneIntervals(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}
	out := make([]Interval, len(in))
	for i, iv := range in {
		out[i] = iv.clone()
	}
	return out
}

func (d *Day) clone() *Day {
	if d == nil {
		return nil
	}
	out := *d
	out.Confirmed = cloneIntervals(d.Confirmed)
	out.Staged = cloneIntervals(d.Staged)
	out.History = append([]HistoryEntry(nil), d.History...)
	return &out
}

func intervalsEqual(a, b []Interval) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Date != b[i].Date || a[i].Start != b[i].Start ||
			a[i].End != b[i].End || a[i].Project != b[i].Project ||
			a[i].Classification != b[i].Classification ||
			a[i].Description != b[i].Description ||
			a[i].RemoteClassificationID != b[i].RemoteClassificationID ||
			a[i].RemoteCatalogID != b[i].RemoteCatalogID ||
			a[i].MigratedClassificationID != b[i].MigratedClassificationID ||
			len(a[i].EvidenceRefs) != len(b[i].EvidenceRefs) {
			return false
		}
		for j := range a[i].EvidenceRefs {
			if a[i].EvidenceRefs[j] != b[i].EvidenceRefs[j] {
				return false
			}
		}
	}
	return true
}

// normalizeEvidenceRefs validates and de-duplicates evidence references. The
// count of references never changes duration.
func normalizeEvidenceRefs(refs []string) ([]string, error) {
	if len(refs) > maxEvidenceRefs {
		return nil, newError(CodeInvalidInterval, "too many evidence references")
	}
	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || len([]rune(ref)) > maxEvidenceRefRunes || controlRune.MatchString(ref) {
			return nil, newError(CodeInvalidInterval, "evidence reference is empty or invalid")
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out, nil
}

func allocationStart(a Allocation) int {
	m, _ := parseClock(a.Start)
	return m
}

// segmentOn returns the rendered segment for date, if iv contributes to it. The
// allocation is computed in the origin day's timezone.
func segmentOn(iv Interval, originDate, date string, loc *time.Location) (Segment, bool, error) {
	allocations, err := iv.ResolvedAllocations(loc)
	if err != nil {
		return Segment{}, false, err
	}
	for _, a := range allocations {
		if a.Date != date {
			continue
		}
		seg := Segment{
			IntervalID:      iv.ID,
			Date:            date,
			Start:           a.Start,
			End:             a.End,
			Minutes:         a.Minutes,
			Project:         iv.Project,
			Classification:  iv.Classification,
			Description:     iv.Description,
			EvidenceRefs:    append([]string(nil), iv.EvidenceRefs...),
			OriginDate:      originDate,
			Locked:          originDate != date,
			CrossesMidnight: iv.CrossesMidnight(),
			Allocations:     allocations,
		}
		if len(seg.Allocations) <= 1 {
			seg.Allocations = nil
		}
		return seg, true, nil
	}
	return Segment{}, false, nil
}

func sortSegments(segs []Segment) {
	sort.SliceStable(segs, func(i, j int) bool {
		if segs[i].Date != segs[j].Date {
			return segs[i].Date < segs[j].Date
		}
		si, sj := allocationStart(Allocation{Start: segs[i].Start}), allocationStart(Allocation{Start: segs[j].Start})
		if si != sj {
			return si < sj
		}
		return segs[i].IntervalID < segs[j].IntervalID
	})
}

func fmtID(id string) string {
	if id == "" {
		return "(unidentified)"
	}
	return id
}

func overlapError(date, first, second string) *AppError {
	return newError(CodeOverlap, fmt.Sprintf(
		"overlap on %s between %s and %s; adjust one interval, or save the day that owns the other interval, before continuing",
		date, fmtID(first), fmtID(second)))
}

// ---------------------------------------------------------------------------
// R2 evidence model. Evidence is provider-sourced review material for one
// opened day. It never contains worked durations: Topic Candidates and
// Proposed Meetings always have unknown worked duration, and attendance is a
// separate explicit local decision, never a classifier or collection result.
// ---------------------------------------------------------------------------

// Bounded evidence caps. They are deliberately fixed, documented constants, not
// configuration, so collection output cannot grow without bound.
const (
	maxEvidenceTopics    = 200
	maxEvidenceMeetings  = 200
	maxTopicItems        = 50
	maxGithubFamilyPages = 20
	// maxSearchResultPages bounds the three search endpoint families (the
	// authored-PR search, the merged-PR search and the submitted-review
	// candidate search). The search endpoint's pagination semantics differ
	// from the rest endpoints (documented 1,000-result bound,
	// incomplete_results flag), so it carries its own documented ceiling
	// instead of the shared maxGithubFamilyPages; reaching it makes the
	// family incomplete, never silently exhausted.
	maxSearchResultPages = 10
	// maxIssueTargetLookups bounds how many documented issue lookups
	// (GET /repos/{o}/{r}/issues/{n}, the only authoritative pull-request
	// discriminator for an issue comment) the issue_comments family issues per
	// repository per attempt. Lookups are cached per run, so one PR
	// conversation with many comments costs one lookup. Exceeding the ceiling
	// leaves the affected items classified as issue comments and makes the
	// family incomplete with the honest count — never a guess, never a family
	// failure.
	maxIssueTargetLookups = 50
	// maxErrorBodyBytes bounds how much of a non-200 provider response body is
	// read for the sanitized family diagnostic (see sanitizeHTTPDiagnostic in
	// github.go). The snippet is rendered sanitized and bounded: URLs and
	// control characters are stripped, but credential-shaped text outside a URL
	// is NOT redacted, so callers must not treat the detail as credential-free.
	maxErrorBodyBytes = 1024
	// maxReviewPRCandidates bounds how many pull requests the submitted-review
	// family will expand per repository in one attempt. It is a documented
	// collector bound, not a provider limit: exceeding it makes the family
	// incomplete, never silently smaller.
	maxReviewPRCandidates = 100
	maxGithubBodyBytes    = 4 << 20
	maxICSBodyBytes       = 8 << 20
	collectHTTPTimeout    = 15 * time.Second
	maxTitleRunes         = 240
	maxDetailRunes        = 400
	// maxDraftTopicCandidates bounds how many topic_candidates lines one draft
	// block renders before an honest "+N more" overflow line (N counts the
	// omitted rendered lines). It is a draft-rendering bound only: durable
	// evidence state keeps every candidate, and recorded entries are unchanged.
	maxDraftTopicCandidates = 12
	// maxRemoteEntryMinutes is the Gradion remote's live-verified per-entry
	// duration policy: log_time refuses entries longer than four hours
	// (issue #167 — two deterministic refusals of a 479-minute entry on
	// 2026-10-25, remote verified empty after each). The stage derivation
	// splits longer recorded blocks into contiguous chunks of at most this
	// many minutes; previewPlan additionally blocks any operation that
	// exceeds it, so an over-long interval can never reach approve silently.
	maxRemoteEntryMinutes = 240
	// maxTopicOverflowItems bounds how many items beyond the maxTopicItems
	// retained Items cap one topic keeps durably as overflow evidence
	// (issue #146). It keeps the audit trail behind a truncated topic
	// recoverable without letting any one topic's state grow without bound;
	// beyond it the loss is counted honestly on the topic, never silent.
	maxTopicOverflowItems = 150
	// maxTotalOverflowItems bounds the total overflow items retained across
	// all topics of one collection attempt, so many truncated topics cannot
	// multiply the per-topic ceiling into unbounded state growth. The pool is
	// filled in item arrival order (the de-duplicated collector order), so
	// later topics lose their overflow first; beyond it the loss is counted
	// honestly on the affected topics, never silent.
	maxTotalOverflowItems = 600
)

// Evidence source identifiers, attempt states, and decision words.
const (
	SourceGithub   = "github"
	SourceCalendar = "calendar"

	StatusDisabled           = "disabled"
	StatusComplete           = "complete"
	StatusIncomplete         = "incomplete"
	StatusFailed             = "failed"
	StatusMissingCredentials = "missing_credentials"
	// StatusWindowed is a FamilyReport-only status (never a source attempt
	// status): the family's coverage is a bounded recent window by the
	// provider's documented design — for the repo_events timeline, only the
	// past 30 days and up to 300 events. It is honest bounded-by-design
	// coverage, not a collection limitation, and the aggregate source detail
	// counts only `incomplete` and `failed` families against it.
	StatusWindowed = "windowed"

	DecisionPending  = "pending"
	DecisionAttended = "attended"
	DecisionSkipped  = "skipped"
)

// SourceAttempt is one recorded collection attempt for one enabled source,
// bound to a collection id so acknowledgment can never become standing
// permission.
type SourceAttempt struct {
	Source         string         `json:"source"`
	CollectionID   string         `json:"collection_id"`
	AttemptedAt    string         `json:"attempted_at"`
	Status         string         `json:"status"`
	Detail         string         `json:"detail,omitempty"`
	Families       []FamilyReport `json:"families,omitempty"`
	Acknowledged   bool           `json:"acknowledged"`
	AcknowledgedAt string         `json:"acknowledged_at,omitempty"`
}

// FamilyReport is the per-activity-family completeness record for GitHub. It
// preserves the endpoint family, the bound that stopped it, and how many
// attributable items were retained.
type FamilyReport struct {
	Family      string `json:"family"` // commits|authored_pull_requests|merged_pull_requests|submitted_pr_reviews|pr_review_comments|issue_comments|repo_events
	QueriedRepo string `json:"queried_repo"`
	Count       int    `json:"count"`
	Status      string `json:"status"` // complete|incomplete|failed|empty|windowed
	Detail      string `json:"detail,omitempty"`
}

// EvidenceItem is one attributable provider event with provenance. Provider
// event identity is kept even when the same event is seen through more than
// one endpoint family: duplicates are marked, never deleted, and never grow
// into minutes. Resource is the provider resource identity (for example
// "pr:owner/name:12" or "issue:owner/name:7") used for grouping; it is kept
// separately from the human-readable title so grouping never depends on
// rendered display strings.
type EvidenceItem struct {
	Ref        string `json:"ref"`
	Kind       string `json:"kind"`
	Actor      string `json:"actor,omitempty"`
	Time       string `json:"time"`
	Repo       string `json:"repo,omitempty"`
	Resource   string `json:"resource,omitempty"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	Provenance string `json:"provenance,omitempty"`
}

// TopicCandidate groups related attributable evidence. Worked duration is
// unknown by design; timestamps and gaps never become working intervals.
// Truncated/DroppedItems make honest visibility: when a topic had more
// provider items than the fixed Item cap, the fact is recorded here with a
// truthful count rather than a silent no-op. Items beyond the retained Items
// cap are kept durably as OverflowItems up to the fixed overflow ceilings
// (issue #146); DroppedItems counts only items retained in neither form —
// the honest per-topic loss. For state written before overflow retention,
// DroppedItems kept its original beyond-the-Items-cap meaning, which under
// the current reading is the same count (nothing beyond the cap was
// retained).
type TopicCandidate struct {
	Ref       string         `json:"ref"`
	Title     string         `json:"title"`
	Provider  string         `json:"provider"`
	Items     []EvidenceItem `json:"items"`
	FirstAt   string         `json:"first_at,omitempty"`
	LastAt    string         `json:"last_at,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
	// DroppedItems counts items retained in neither Items nor OverflowItems —
	// the honest per-topic loss beyond the fixed overflow ceilings. For state
	// written before overflow retention it kept its original
	// beyond-the-Items-cap meaning, which under the current reading is the
	// same count (nothing beyond the cap was retained then).
	DroppedItems int `json:"dropped_items,omitempty"`
	// OverflowItems holds attributable items beyond the maxTopicItems
	// retained Items cap, retained durably up to maxTopicOverflowItems per
	// topic and maxTotalOverflowItems per collection attempt. They never
	// extend FirstAt/LastAt: those stay computed from the retained Items so
	// the draft's topic-to-block span mapping is unchanged by retention.
	OverflowItems []EvidenceItem `json:"overflow_items,omitempty"`
}

// ProposedMeeting is a calendar proposal. It is never confirmed work. Status,
// the configured user's invitation answer, and the explicit local attendance
// decision are distinct fields that must not be merged.
type ProposedMeeting struct {
	Ref        string `json:"ref"`
	UID        string `json:"uid"`
	Title      string `json:"title,omitempty"`
	Start      string `json:"start"`
	End        string `json:"end,omitempty"`
	AllDay     bool   `json:"all_day"`
	Status     string `json:"status,omitempty"`
	Attendance string `json:"attendance,omitempty"`
	Recurrence string `json:"recurrence,omitempty"`
	Decision   string `json:"decision,omitempty"`
}

// DayEvidence is the persisted review state of one day's collection. It is
// written atomically by collect/acknowledge/decide operations and is never
// part of confirmed work, revisions, or save receipts.
type DayEvidence struct {
	Attempts []SourceAttempt   `json:"attempts,omitempty"`
	Topics   []TopicCandidate  `json:"topics,omitempty"`
	Meetings []ProposedMeeting `json:"meetings,omitempty"`
}

// composedSourceStatus is the review-visible status of one source, combining
// the last attempt (if any) with configuration-disabled state.
type ComposedSourceStatus struct {
	Source       string `json:"source"`
	Enabled      bool   `json:"enabled"`
	Status       string `json:"status"`
	Detail       string `json:"detail,omitempty"`
	CollectionID string `json:"collection_id,omitempty"`
	AttemptedAt  string `json:"attempted_at,omitempty"`
	Acknowledged bool   `json:"acknowledged,omitempty"`
}

var (
	githubSourceUserRE = regexp.MustCompile(`^[A-Za-z0-9][-A-Za-z0-9._]*$`)
	githubRepoOwnerRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	githubRepoNameRE   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

func validDecision(d string) bool {
	switch d {
	case DecisionPending, DecisionAttended, DecisionSkipped:
		return true
	}
	return false
}

// validEvidenceStatus enforces the persisted source-attempt status enum,
// fail-closed on load. Family-level statuses (including the repo_events
// steady state "windowed") are report-only: they live in
// SourceAttempt.Families and are not validated here.
func validEvidenceStatus(s string) bool {
	switch s {
	case StatusDisabled, StatusComplete, StatusIncomplete, StatusFailed, StatusMissingCredentials, "empty":
		return true
	}
	return false
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------------------
// R3 durable schema 2: explicit legacy migration, read-only remote
// reconciliation, and immutable plan previews. Nothing in this section grants
// remote write authority, and no migrated approval can become usable.
// ---------------------------------------------------------------------------

// R3 fixed bounds. They are documented constants, not configuration, so a
// preview, receipt or rendered plan cannot grow without bound.
const (
	maxMigrationDates     = 62
	maxLegacyRecordBytes  = 32 << 20
	maxMigrationConflicts = 50
	maxRemoteEntries      = 1000
	maxCatalogOptions     = 20
	maxStoredCatalogs     = 20
	maxStoredPlans        = 200
	maxResolutions        = 500
)

// R4 native approval/submission bounds. The approval ledger grows only with
// explicit operator approvals and is never pruned (a consumed or revoked
// approval is immutable audit); intents and executions are pruned only when
// entirely resolved or terminal, never while they still block replay.
const (
	maxStoredApprovals  = 2000
	maxStoredIntents    = 1000
	maxStoredExecutions = 1000
)

// Migration audit capacity bounds. A candidate that exceeds one is rejected with
// an explicit migration_capacity conflict before activation; audit rows are never
// silently truncated. Limits are per category and per activated receipt.
const (
	maxMigrationEvidenceRows = 2000
	maxMigrationIntervalRows = 2000
	maxMigrationPlanRows     = 500
	maxMigrationApprovalRows = 500
)

// Migration decision words for one legacy source file.
const (
	MigrationNoChange = "no_change"
	MigrationApplied  = "applied"
)

// Remote resolution decisions. adopt binds a local interval to a specific
// observed remote entry; accept_observed accepts the observed content for the
// SAME already-mapped remote identity; foreign acknowledges a remote entry with
// no local counterpart (it never creates a mapping and never deletes anything).
const (
	DecisionAdopt          = "adopt"
	DecisionAcceptObserved = "accept_observed"
	DecisionForeign        = "foreign"
)

// Plan states. There is deliberately no approved state in R3.
const (
	PlanReady   = "ready"
	PlanBlocked = "blocked"
)

// MigrationState is the durable migration section. Receipts are append-only
// audit records; Sources maps a repeat-safe source identity to its receipt so a
// repeated import of identical bytes is a no-change result. Carries preserves
// legacy cross-midnight carry fragments as UNCONFIRMED blocks (they are never
// promoted into confirmed work).
type MigrationState struct {
	Receipts []MigrationReceipt `json:"receipts,omitempty"`
	Sources  map[string]string  `json:"sources,omitempty"`
	Carries  []MigratedCarry    `json:"carries,omitempty"`
}

// MigrationReceipt is the durable audit record of one activated batch. It stores
// the source hashes and the legacy values needed for audit, never raw provider
// documents, credentials or authenticated URLs. Per-date review history is a
// list, not one last-day pointer; IDMapping is retained only for compatibility
// with earlier schema-2 receipts and is superseded by IDCrosswalk.
type MigrationReceipt struct {
	MigrationID          string                `json:"migration_id"`
	SourceIdentity       string                `json:"source_identity"`
	Dates                []string              `json:"dates"`
	Timezone             string                `json:"timezone"`
	SourceHashes         map[string]string     `json:"source_hashes"`
	AppliedAt            string                `json:"applied_at"`
	Intervals            int                   `json:"intervals"`
	Classified           int                   `json:"classified"`
	Mappings             int                   `json:"mappings"`
	Outcomes             int                   `json:"outcomes"`
	Carries              int                   `json:"carries"`
	LegacyPlans          int                   `json:"legacy_plans"`
	LegacyApprovals      int                   `json:"legacy_approvals"`
	IDMapping            map[string]string     `json:"id_mapping,omitempty"`
	IDCrosswalk          []LegacyIDCrosswalk   `json:"id_crosswalk,omitempty"`
	LegacyRevisions      map[string]int        `json:"legacy_revisions,omitempty"`
	LegacyUpdatedAt      map[string]string     `json:"legacy_updated_at,omitempty"`
	LegacyIntervals      []LegacyIntervalAudit `json:"legacy_intervals,omitempty"`
	LegacyPlanAudits     []LegacyPlanAudit     `json:"legacy_plan_audits,omitempty"`
	LegacyApprovalsAudit []LegacyApprovalAudit `json:"legacy_approval_audits,omitempty"`
	LegacyEvidence       []LegacyEvidenceAudit `json:"legacy_evidence,omitempty"`
	// LegacyReviews preserves every reviewed source date. LegacyReview is the
	// earlier single-pointer shape, retained only to load existing receipts.
	LegacyReviews []LegacyReviewAudit `json:"legacy_reviews,omitempty"`
	LegacyReview  *LegacyReviewAudit  `json:"legacy_review,omitempty"`
	// LegacyCollectedEvidence counts legacy raw collected-evidence documents
	// that were NOT imported. Raw provider documents never enter state, and
	// archival provenance never enables a retired collector.
	LegacyCollectedEvidence int      `json:"legacy_collected_evidence,omitempty"`
	Revalidate              []string `json:"revalidate,omitempty"`
}

// LegacyIDCrosswalk is the date-scoped legacy-to-new interval identity mapping.
// The same legacy ID on two dates is two distinct entries, never one collapsed
// map value.
type LegacyIDCrosswalk struct {
	Date     string `json:"date"`
	LegacyID string `json:"legacy_id"`
	NewID    string `json:"new_id"`
}

// LegacyEvidenceAudit preserves a non-secret legacy provenance pointer (date,
// source, timestamp, ref, title) so the interval evidence IDs keep their meaning.
type LegacyEvidenceAudit struct {
	ID         string `json:"id"`
	Date       string `json:"date,omitempty"`
	Source     string `json:"source,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
	EndTime    string `json:"end_time,omitempty"`
	Title      string `json:"title,omitempty"`
	Ref        string `json:"ref,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
}

// LegacyReviewAudit preserves one source date's legacy review outcome as inert
// history, including the authored exclusion reasons/timestamps and attendance
// decisions rather than counts alone.
type LegacyReviewAudit struct {
	Date           string                  `json:"date,omitempty"`
	Reviewed       bool                    `json:"reviewed"`
	Empty          bool                    `json:"empty"`
	ReviewedAt     string                  `json:"reviewed_at,omitempty"`
	Excluded       int                     `json:"excluded"`
	Attendance     int                     `json:"attendance"`
	ExcludedRows   []LegacyExcludedAudit   `json:"excluded_rows,omitempty"`
	AttendanceRows []LegacyAttendanceAudit `json:"attendance_rows,omitempty"`
	Acknowledged   []string                `json:"acknowledged_sources,omitempty"`
}

// LegacyExcludedAudit preserves one authored review exclusion.
type LegacyExcludedAudit struct {
	EvidenceID string `json:"evidence_id"`
	Reason     string `json:"reason,omitempty"`
	ExcludedAt string `json:"excluded_at,omitempty"`
}

// LegacyAttendanceAudit preserves one attendance decision and when it was made.
type LegacyAttendanceAudit struct {
	EvidenceID string `json:"evidence_id"`
	Attended   bool   `json:"attended"`
	DecidedAt  string `json:"decided_at,omitempty"`
}

// LegacyIntervalAudit preserves legacy interval attributes the timesheet model
// does not represent, without fabricating an interval or a history snapshot.
// Date and the original project/classification keep the audit date-associated
// and faithful to the authored source.
type LegacyIntervalAudit struct {
	NewID            string `json:"new_id"`
	LegacyID         string `json:"legacy_id,omitempty"`
	Date             string `json:"date,omitempty"`
	ConfirmedAt      string `json:"confirmed_at,omitempty"`
	Revision         int    `json:"revision,omitempty"`
	Attendance       bool   `json:"attendance,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Manual           bool   `json:"manual,omitempty"`
	Carryover        bool   `json:"carryover,omitempty"`
	SplitGroupID     string `json:"split_group_id,omitempty"`
	PartIndex        int    `json:"part_index,omitempty"`
	LegacyProject    string `json:"legacy_project,omitempty"`
	LegacyClassID    string `json:"legacy_classification_id,omitempty"`
	LegacyClassCode  string `json:"legacy_classification_code,omitempty"`
	LegacyClassLabel string `json:"legacy_classification_name,omitempty"`
}

// LegacyPlanAudit preserves a historical submission plan strictly as inert
// history, including its operation contents and the preconditions it was bound
// to. It cannot be approved, re-approved or executed.
type LegacyPlanAudit struct {
	PlanID           string                 `json:"plan_id,omitempty"`
	Date             string                 `json:"date,omitempty"`
	Operations       int                    `json:"operations"`
	OperationRows    []LegacyOperationAudit `json:"operation_rows,omitempty"`
	LocalFingerprint string                 `json:"local_fingerprint,omitempty"`
	RecordRevision   int                    `json:"record_revision,omitempty"`
	RemoteObservedAt string                 `json:"remote_observed_at,omitempty"`
	RemoteComplete   bool                   `json:"remote_complete,omitempty"`
	CreatedAt        string                 `json:"created_at,omitempty"`
	Inert            bool                   `json:"inert"`
}

// LegacyOperationAudit preserves one historical plan operation as inert text.
type LegacyOperationAudit struct {
	LocalID          string             `json:"local_id,omitempty"`
	Kind             string             `json:"kind,omitempty"`
	RemoteID         string             `json:"remote_id,omitempty"`
	Date             string             `json:"date,omitempty"`
	StartTime        string             `json:"start_time,omitempty"`
	EndTime          string             `json:"end_time,omitempty"`
	ClassificationID string             `json:"classification_id,omitempty"`
	Description      string             `json:"description,omitempty"`
	Task             string             `json:"task,omitempty"`
	Billable         *bool              `json:"billable,omitempty"`
	Reason           string             `json:"reason,omitempty"`
	ExpectedRemote   *LegacyRemoteAudit `json:"expected_remote,omitempty"`
}

// LegacyRemoteAudit preserves a historical plan's observed-remote precondition.
// It is structured content, never a raw provider document.
type LegacyRemoteAudit struct {
	RemoteID         string `json:"remote_id"`
	Date             string `json:"date"`
	StartTime        string `json:"start_time"`
	EndTime          string `json:"end_time"`
	ClassificationID string `json:"classification_id,omitempty"`
	Description      string `json:"description,omitempty"`
	Task             string `json:"task,omitempty"`
	Billable         *bool  `json:"billable,omitempty"`
	ObservedAt       string `json:"observed_at,omitempty"`
	ContentObserved  bool   `json:"content_observed,omitempty"`
}

// LegacyApprovalAudit preserves a historical approval (and any revocation)
// strictly as inert history. Usable is always false: R3 has no approval or
// submission operation.
type LegacyApprovalAudit struct {
	ApprovalID string `json:"approval_id,omitempty"`
	PlanID     string `json:"plan_id,omitempty"`
	Date       string `json:"date,omitempty"`
	// RecordRevision and RemoteObservedAt preserve the approval binding's own
	// historical values; they are never inferred from the linked plan, whose
	// revisions and observation timestamps may differ.
	RecordRevision    int    `json:"record_revision,omitempty"`
	RemoteObservedAt  string `json:"remote_observed_at,omitempty"`
	ApprovedAt        string `json:"approved_at,omitempty"`
	Superseded        bool   `json:"superseded,omitempty"`
	InvalidatedAt     string `json:"invalidated_at,omitempty"`
	InvalidatedReason string `json:"invalidated_reason,omitempty"`
	Inert             bool   `json:"inert"`
	Usable            bool   `json:"usable"`
}

// MigratedCarry is a legacy cross-midnight carry fragment preserved as an
// UNCONFIRMED block on its later date. It never becomes confirmed work.
type MigratedCarry struct {
	FragmentID   string   `json:"fragment_id"`
	SourceDate   string   `json:"source_date"`
	Date         string   `json:"date"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	Minutes      int      `json:"minutes"`
	SplitGroupID string   `json:"split_group_id,omitempty"`
	PartIndex    int      `json:"part_index,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	Project      string   `json:"project,omitempty"`
	Description  string   `json:"description,omitempty"`
	EvidenceIDs  []string `json:"evidence_ids,omitempty"`
	Confirmed    bool     `json:"confirmed"`
	MigrationID  string   `json:"migration_id,omitempty"`
}

// RemoteState is the durable read-only remote section: one-to-one stable
// bindings, the latest observation per date, explicit human resolutions,
// preserved write outcomes, and the bounded classification catalogs that
// selections reference. Its presence never authorizes a remote write.
type RemoteState struct {
	Bindings     []RemoteBinding               `json:"bindings,omitempty"`
	Observations map[string]*RemoteObservation `json:"observations,omitempty"`
	Resolutions  []RemoteResolution            `json:"resolutions,omitempty"`
	Outcomes     []WriteOutcome                `json:"outcomes,omitempty"`
	Catalogs     map[string]*CatalogRecord     `json:"catalogs,omitempty"`
}

// RemoteEntry is one observed remote entry's CONTENT. ContentObserved is true
// only when it came from a structured read; unknown fields (for example billable
// state the read does not expose) stay unknown rather than false.
type RemoteEntry struct {
	RemoteID         string `json:"remote_id"`
	Date             string `json:"date"`
	StartTime        string `json:"start_time"`
	EndTime          string `json:"end_time"`
	ClassificationID string `json:"classification_id,omitempty"`
	Description      string `json:"description,omitempty"`
	Task             string `json:"task,omitempty"`
	Status           string `json:"status,omitempty"`
	DurationMinutes  *int   `json:"duration_minutes,omitempty"`
	Billable         *bool  `json:"billable,omitempty"`
	ContentObserved  bool   `json:"content_observed"`
}

// RemoteBinding is one stable local-to-remote identity. The binding fields are
// the last binding event's local intent; LastObserved is the single carrier of
// observed remote CONTENT truth. A bare read never creates or moves either.
type RemoteBinding struct {
	LocalID          string       `json:"local_id"`
	RemoteID         string       `json:"remote_id"`
	Date             string       `json:"date"`
	StartTime        string       `json:"start_time"`
	EndTime          string       `json:"end_time"`
	ClassificationID string       `json:"classification_id,omitempty"`
	Description      string       `json:"description,omitempty"`
	LastObserved     *RemoteEntry `json:"last_observed,omitempty"`
	// BaselineFields names the observed-content fields the last observation
	// actually carried. A legacy record did not record every field the current
	// read exposes, so an unrecorded field is unknown, not a contradiction.
	// Empty means the baseline is a full current observation.
	BaselineFields []string `json:"baseline_fields,omitempty"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	Origin         string   `json:"origin,omitempty"`
	MigrationID    string   `json:"migration_id,omitempty"`
}

// RemoteObservation is the durable record of the latest read for one date. A
// failed read replaces the previous record with Failed=true so an old complete
// observation can never stand in as current proof of absence.
type RemoteObservation struct {
	ObservationID string        `json:"observation_id"`
	Date          string        `json:"date"`
	ObservedAt    string        `json:"observed_at"`
	Complete      bool          `json:"complete"`
	Failed        bool          `json:"failed,omitempty"`
	Detail        string        `json:"detail,omitempty"`
	Coverage      []string      `json:"coverage,omitempty"`
	Duplicates    int           `json:"duplicates,omitempty"`
	Entries       []RemoteEntry `json:"entries,omitempty"`
}

// RemoteResolution records how the human appears to have described it: an
// explicit identity/content decision bound to one observation.
type RemoteResolution struct {
	Date                string `json:"date"`
	Revision            int    `json:"revision"`
	ObservationID       string `json:"observation_id"`
	Decision            string `json:"decision"`
	RemoteID            string `json:"remote_id"`
	LocalID             string `json:"local_id,omitempty"`
	ObservationComplete bool   `json:"observation_complete"`
	At                  string `json:"at"`
}

// WriteOutcome is a preserved legacy operation outcome. Unresolved rows remain
// durable blockers; R3 never replays, retries or automatically clears them.
type WriteOutcome struct {
	OperationKey     string `json:"operation_key"`
	LocalID          string `json:"local_id,omitempty"`
	RemoteID         string `json:"remote_id,omitempty"`
	Action           string `json:"action,omitempty"`
	Status           string `json:"status"`
	Date             string `json:"date,omitempty"`
	StartTime        string `json:"start_time,omitempty"`
	EndTime          string `json:"end_time,omitempty"`
	Description      string `json:"description,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
	OriginErrorCode  string `json:"origin_error_code,omitempty"`
	ClassificationID string `json:"classification_id,omitempty"`
	Resolved         bool   `json:"resolved"`
	// R4 explicit human resolution of an unresolved migrated outcome. The
	// original uncertainty/reason above is never erased; these fields only
	// record the addressed decision as audit (adopt | not_applied).
	Resolution              string `json:"resolution,omitempty"`
	ResolvedAt              string `json:"resolved_at,omitempty"`
	ResolutionObservationID string `json:"resolution_observation_id,omitempty"`
	RecordedAt              string `json:"recorded_at,omitempty"`
	IntentAt                string `json:"intent_at,omitempty"`
	MigrationID             string `json:"migration_id,omitempty"`
}

// WriteIntent is one durable native write attempt. A row is committed with
// status in_flight BEFORE its single dispatch; only an explicitly verified
// outcome (or an explicit human resolution) ever moves it past in_flight, and
// an in_flight/uncertain/unidentified row always blocks replay until it is
// addressed. Operation is the exact approved operation, including the edit
// audit reason; the request never supplies or alters it.
type WriteIntent struct {
	WriteRef    string `json:"write_ref"`
	ExecutionID string `json:"execution_id"`
	ApprovalID  string `json:"approval_id"`
	PlanID      string `json:"plan_id"`
	Date        string `json:"date"`
	OpIndex     int    `json:"op_index"`
	LocalID     string `json:"local_id"`
	Action      string `json:"action"`
	RemoteID    string `json:"remote_id,omitempty"`
	// AcknowledgedID is an identity the provider acknowledged, when one arrived.
	// It is never derived, guessed or copied from planned content, and it is
	// never presented as a verified outcome: identity alone cannot confirm.
	AcknowledgedID string        `json:"acknowledged_id,omitempty"`
	OpDigest       string        `json:"op_digest"`
	Operation      PlanOperation `json:"operation"`
	// Preconditions is the plan's remote fingerprint (the expected remote
	// content/coverage preconditions the approval was bound to).
	Preconditions string       `json:"preconditions"`
	Status        string       `json:"status"`
	ErrorCode     string       `json:"error_code,omitempty"`
	Observed      *RemoteEntry `json:"observed,omitempty"`
	AttemptedAt   string       `json:"attempted_at,omitempty"`
	RecordedAt    string       `json:"recorded_at,omitempty"`
	// Explicit human resolution (resolve_write). Original attempt audit is
	// never erased by it.
	Resolved                bool   `json:"resolved,omitempty"`
	Resolution              string `json:"resolution,omitempty"`
	ResolvedAt              string `json:"resolved_at,omitempty"`
	ResolutionObservationID string `json:"resolution_observation_id,omitempty"`
}

// unresolved reports whether a native attempt still needs explicit
// reconciliation. It is the single native predicate: the restart gate, plan
// readiness, submission_status and resolve_write all defer to it.
func (i *WriteIntent) unresolved() bool {
	if i.Resolved {
		return false
	}
	switch i.Status {
	case IntentInFlight, IntentUncertain, IntentUnidentified:
		return true
	}
	return false
}

// ExecutionRecord is one native submission audit row. State is lifecycle only;
// it never authorizes anything and never becomes approval authority.
type ExecutionRecord struct {
	ExecutionID string `json:"execution_id"`
	ApprovalID  string `json:"approval_id"`
	PlanID      string `json:"plan_id"`
	Date        string `json:"date"`
	State       string `json:"state"`
	Attempts    int    `json:"attempts"`
	Confirmed   int    `json:"confirmed"`
	Unresolved  int    `json:"unresolved"`
	Unattempted int    `json:"unattempted"`
	Outcome     string `json:"outcome,omitempty"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	NextAction  string `json:"next_action,omitempty"`
}

// CatalogRecord is a bounded authoritative classification fetch. A catalog that
// is not Complete can never prove an option absent.
type CatalogRecord struct {
	CatalogID  string                 `json:"catalog_id"`
	Query      string                 `json:"query,omitempty"`
	Options    []ClassificationOption `json:"options"`
	Complete   bool                   `json:"complete"`
	ObservedAt string                 `json:"observed_at"`
}

// ClassificationOption is one returned catalog option. Code and ActivityType
// come from the tool's text rendering; ActivityType is display/validation only
// and is never sent to a write tool (there is no write in R3).
type ClassificationOption struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	Code         string `json:"code,omitempty"`
	ActivityType string `json:"activity_type,omitempty"`
}

// ClassificationChoice is the bound relevant semantic identity of one
// explicitly selected classification option: the exact id/code/name/activity
// tuple the operator chose, which every later fresh authoritative proof must
// still match before anything references it (R4F1). Empty reference fields are
// never silently skipped: an option whose selected semantics cannot be
// established is fail-closed, never substituted by name, code or default.
type ClassificationChoice struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	Code         string `json:"code,omitempty"`
	ActivityType string `json:"activity_type,omitempty"`
}

// choiceFromOption trims a catalog option into its bound semantic identity.
func choiceFromOption(o ClassificationOption) ClassificationChoice {
	return ClassificationChoice{
		ID:           strings.TrimSpace(o.ID),
		Name:         strings.TrimSpace(o.Name),
		Code:         strings.TrimSpace(o.Code),
		ActivityType: strings.TrimSpace(o.ActivityType),
	}
}

// choiceEqual compares the RELEVANT content of two references to one selected
// option. A relevant change (name, code or activity type) is meaningful and
// must force reselection; other catalog content, queries and audit timestamps
// never enter the comparison, so unrelated rows do not invalidate a choice.
func choiceEqual(a, b ClassificationChoice) bool {
	return a.ID == b.ID && a.Name == b.Name &&
		a.Code == b.Code && a.ActivityType == b.ActivityType
}

// Blocker is one explicit reason a plan or reconciliation is not ready. It is
// display and audit data; it never mutates work or authorizes a write.
type Blocker struct {
	Code     string `json:"code"`
	Date     string `json:"date,omitempty"`
	LocalID  string `json:"local_id,omitempty"`
	RemoteID string `json:"remote_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// PlanOperation is one exact previewed create or edit. RemoteID is empty for a
// create. ExpectedRemote is the observation the edit's precondition is bound to.
// There is deliberately no delete kind.
type PlanOperation struct {
	LocalID            string         `json:"local_id"`
	Kind               string         `json:"kind"` // create|edit
	RemoteID           string         `json:"remote_id,omitempty"`
	Date               string         `json:"date"`
	StartTime          string         `json:"start_time"`
	EndTime            string         `json:"end_time"`
	Minutes            int            `json:"minutes"`
	ClassificationID   string         `json:"classification_id"`
	Description        string         `json:"description"`
	Task               string         `json:"task,omitempty"`
	CatalogID          string         `json:"catalog_id,omitempty"`
	CrossesMidnight    bool           `json:"crosses_midnight,omitempty"`
	OriginDates        []string       `json:"origin_dates,omitempty"`
	OriginRevisions    map[string]int `json:"origin_revisions,omitempty"`
	ExpectedRemote     *RemoteEntry   `json:"expected_remote,omitempty"`
	PreserveRemoteOnly []string       `json:"preserve_remote_only,omitempty"`
	// Reason is a previewed edit's required audit reason. It is rendered by the
	// binary, never supplied by a request, and is part of the operation's exact
	// approved body and canonical content. A create has no reason.
	Reason string `json:"reason,omitempty"`
	// SelectedChoice is the explicitly selected classification's relevant
	// semantic identity (id/code/name/activity type) resolved from the stored
	// selection reference (R4F1). It is canonical plan content and part of the
	// approval's precondition proof.
	SelectedChoice *ClassificationChoice `json:"selected_choice,omitempty"`
}

// PlanRecord is the immutable persisted preview. State distinguishes ready
// from blocked; the record deliberately carries NO authorization flag. Approval
// authority lives only in the native approval ledger: whether a plan has been
// approved is a derived ledger query, never a second durable truth. This
// removes the obsolete always-false persisted R3 flags through the schema-2
// conversion, so an old plan record in memory never becomes "approved".
type PlanRecord struct {
	PlanID             string            `json:"plan_id"`
	Date               string            `json:"date"`
	State              string            `json:"state"`
	RemoteObservation  string            `json:"remote_observation,omitempty"`
	RemoteObservations map[string]string `json:"remote_observations,omitempty"`
	RemoteComplete     bool              `json:"remote_complete"`
	RemoteFingerprint  string            `json:"remote_fingerprint,omitempty"`
	SourceRevisions    map[string]int    `json:"source_revisions,omitempty"`
	Operations         []PlanOperation   `json:"operations"`
	Blockers           []Blocker         `json:"blockers,omitempty"`
	SurfacedRemotes    []RemoteEntry     `json:"surfaced_remotes,omitempty"`
	CreatedAt          string            `json:"created_at"`
}

// cloneState deep-copies a durable snapshot. Callers build a candidate from it
// so a pre-activation failure can restore the exact previous state.
func (s *State) cloneState() *State {
	if s == nil {
		return nil
	}
	out := &State{Schema: s.Schema, SavedAt: s.SavedAt}
	out.Days = make(map[string]*Day, len(s.Days))
	for k, v := range s.Days {
		out.Days[k] = v.clone()
	}
	out.Evidence = make(map[string]*DayEvidence, len(s.Evidence))
	for k, v := range s.Evidence {
		if v == nil {
			out.Evidence[k] = nil
			continue
		}
		out.Evidence[k] = v.cloneEvidence()
	}
	out.Migration = s.Migration.cloneMigration()
	out.Remote = s.Remote.cloneRemote()
	out.Plans = make(map[string]*PlanRecord, len(s.Plans))
	for k, v := range s.Plans {
		out.Plans[k] = v.clonePlan()
	}
	out.Approvals = make([]ApprovalRecord, len(s.Approvals))
	for i, a := range s.Approvals {
		c := a
		c.Summary.SourceRevisions = deepCopyIntMap(a.Summary.SourceRevisions)
		c.Summary.RemoteObservations = cloneStringMap(a.Summary.RemoteObservations)
		c.Summary.OperationDigests = append([]string(nil), a.Summary.OperationDigests...)
		c.Summary.Classifications = cloneStringMap(a.Summary.Classifications)
		c.Summary.Choices = cloneClassChoiceMap(a.Summary.Choices)
		out.Approvals[i] = c
	}
	out.Intents = make([]WriteIntent, len(s.Intents))
	for i, it := range s.Intents {
		c := it
		c.Operation = clonePlanOperation(it.Operation)
		c.Observed = cloneEntry(it.Observed)
		out.Intents[i] = c
	}
	out.Executions = append([]ExecutionRecord(nil), s.Executions...)
	return out
}

func (m *MigrationState) cloneMigration() *MigrationState {
	if m == nil {
		return nil
	}
	out := &MigrationState{Receipts: append([]MigrationReceipt(nil), m.Receipts...), Carries: append([]MigratedCarry(nil), m.Carries...)}
	out.Sources = make(map[string]string, len(m.Sources))
	for k, v := range m.Sources {
		out.Sources[k] = v
	}
	for i := range out.Receipts {
		r := &out.Receipts[i]
		r.Dates = append([]string(nil), r.Dates...)
		r.Revalidate = append([]string(nil), r.Revalidate...)
		r.LegacyIntervals = append([]LegacyIntervalAudit(nil), r.LegacyIntervals...)
		r.LegacyPlanAudits = cloneLegacyPlanAudits(r.LegacyPlanAudits)
		r.LegacyApprovalsAudit = append([]LegacyApprovalAudit(nil), r.LegacyApprovalsAudit...)
		r.LegacyEvidence = append([]LegacyEvidenceAudit(nil), r.LegacyEvidence...)
		r.IDCrosswalk = append([]LegacyIDCrosswalk(nil), r.IDCrosswalk...)
		r.LegacyReviews = cloneLegacyReviews(r.LegacyReviews)
		if r.LegacyReview != nil {
			rev := cloneLegacyReview(*r.LegacyReview)
			r.LegacyReview = &rev
		}
		r.SourceHashes = cloneStringMap(r.SourceHashes)
		r.IDMapping = cloneStringMap(r.IDMapping)
		if r.LegacyRevisions != nil {
			r.LegacyRevisions = make(map[string]int, len(r.LegacyRevisions))
			for k, v := range m.Receipts[i].LegacyRevisions {
				r.LegacyRevisions[k] = v
			}
		}
		r.LegacyUpdatedAt = cloneStringMap(r.LegacyUpdatedAt)
	}
	for i := range out.Carries {
		out.Carries[i].EvidenceIDs = append([]string(nil), out.Carries[i].EvidenceIDs...)
	}
	return out
}

func cloneLegacyReview(in LegacyReviewAudit) LegacyReviewAudit {
	out := in
	out.Acknowledged = append([]string(nil), in.Acknowledged...)
	out.ExcludedRows = append([]LegacyExcludedAudit(nil), in.ExcludedRows...)
	out.AttendanceRows = append([]LegacyAttendanceAudit(nil), in.AttendanceRows...)
	return out
}

func cloneLegacyReviews(in []LegacyReviewAudit) []LegacyReviewAudit {
	if in == nil {
		return nil
	}
	out := make([]LegacyReviewAudit, len(in))
	for i := range in {
		out[i] = cloneLegacyReview(in[i])
	}
	return out
}

func cloneLegacyPlanAudits(in []LegacyPlanAudit) []LegacyPlanAudit {
	if in == nil {
		return nil
	}
	out := make([]LegacyPlanAudit, len(in))
	for i := range in {
		out[i] = in[i]
		rows := make([]LegacyOperationAudit, len(in[i].OperationRows))
		for j := range in[i].OperationRows {
			row := in[i].OperationRows[j]
			row.Billable = cloneBool(in[i].OperationRows[j].Billable)
			if row.ExpectedRemote != nil {
				exp := *in[i].OperationRows[j].ExpectedRemote
				exp.Billable = cloneBool(exp.Billable)
				row.ExpectedRemote = &exp
			}
			rows[j] = row
		}
		out[i].OperationRows = rows
	}
	return out
}

func (r *RemoteState) cloneRemote() *RemoteState {
	if r == nil {
		return nil
	}
	out := &RemoteState{}
	out.Bindings = append([]RemoteBinding(nil), r.Bindings...)
	for i := range out.Bindings {
		out.Bindings[i].LastObserved = cloneEntry(out.Bindings[i].LastObserved)
		out.Bindings[i].BaselineFields = append([]string(nil), out.Bindings[i].BaselineFields...)
	}
	out.Resolutions = append([]RemoteResolution(nil), r.Resolutions...)
	out.Outcomes = append([]WriteOutcome(nil), r.Outcomes...)
	out.Observations = make(map[string]*RemoteObservation, len(r.Observations))
	for k, v := range r.Observations {
		if v == nil {
			out.Observations[k] = nil
			continue
		}
		cp := *v
		cp.Coverage = append([]string(nil), v.Coverage...)
		cp.Entries = append([]RemoteEntry(nil), v.Entries...)
		for i := range cp.Entries {
			cp.Entries[i].DurationMinutes = cloneInt(v.Entries[i].DurationMinutes)
			cp.Entries[i].Billable = cloneBool(v.Entries[i].Billable)
		}
		out.Observations[k] = &cp
	}
	out.Catalogs = make(map[string]*CatalogRecord, len(r.Catalogs))
	for k, v := range r.Catalogs {
		if v == nil {
			out.Catalogs[k] = nil
			continue
		}
		cp := *v
		cp.Options = append([]ClassificationOption(nil), v.Options...)
		out.Catalogs[k] = &cp
	}
	return out
}

func (p *PlanRecord) clonePlan() *PlanRecord {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Operations = append([]PlanOperation(nil), p.Operations...)
	for i := range cp.Operations {
		cp.Operations[i].OriginDates = append([]string(nil), p.Operations[i].OriginDates...)
		cp.Operations[i].PreserveRemoteOnly = append([]string(nil), p.Operations[i].PreserveRemoteOnly...)
		cp.Operations[i].ExpectedRemote = cloneEntry(p.Operations[i].ExpectedRemote)
		if p.Operations[i].OriginRevisions != nil {
			cp.Operations[i].OriginRevisions = make(map[string]int, len(p.Operations[i].OriginRevisions))
			for k, v := range p.Operations[i].OriginRevisions {
				cp.Operations[i].OriginRevisions[k] = v
			}
		}
	}
	cp.Blockers = append([]Blocker(nil), p.Blockers...)
	cp.SurfacedRemotes = append([]RemoteEntry(nil), p.SurfacedRemotes...)
	if p.SourceRevisions != nil {
		cp.SourceRevisions = make(map[string]int, len(p.SourceRevisions))
		for k, v := range p.SourceRevisions {
			cp.SourceRevisions[k] = v
		}
	}
	cp.RemoteObservations = cloneStringMap(p.RemoteObservations)
	return &cp
}

func cloneEntry(e *RemoteEntry) *RemoteEntry {
	if e == nil {
		return nil
	}
	cp := *e
	cp.DurationMinutes = cloneInt(e.DurationMinutes)
	cp.Billable = cloneBool(e.Billable)
	return &cp
}

func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	cp := *v
	return &cp
}

func cloneBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	cp := *v
	return &cp
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

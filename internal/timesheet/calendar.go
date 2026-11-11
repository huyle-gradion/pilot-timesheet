package timesheet

//   - Original event timestamps are preserved with their original UTC offsets;
//     UTC "Z" forms keep exact instants.
//   - Invitation/attendance metadata (ATTENDEE PARTSTAT) is reported for the
//     identity named in sources.calendar.user. A declined or cancelled event
//     is still listed as a proposal with those statuses; attendance itself is
//     only ever the user's explicit local decision.
//
// Documented limits: per-event parse failures, unknown TZID names, malformed
// RRULEs, RDATE/EXDATE values that cannot be resolved in their own timezone,
// value forms that do not match the series' DTSTART form, feed totals beyond
// the evidence caps, a bounded (500-instance) per-series expansion count, and
// a feed larger than the read limit make the attempt incomplete instead of
// silently dropping content. A VTIMEZONE-only calendar whose zone is not a
// resolvable IANA name is such a limit. VALARM/FREEBUSY data is not
// interpreted.

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
	"github.com/teambition/rrule-go"
)

type collectCalendarResult struct {
	Status   string
	Detail   string
	Meetings []ProposedMeeting
}

// collectCalendar fetches and expands the configured private feed for the
// recorded day's calendrical window [windowStart, windowEnd). It never echoes
// the feed URL, its credential value, or any part of the fetched document.
func collectCalendar(calUser string, env map[string]string, windowStart, windowEnd time.Time, dayLoc *time.Location) collectCalendarResult {
	raw, present := resolveEnv(env, "GCAL_ICS_URL")
	if !present || strings.TrimSpace(raw) == "" {
		return collectCalendarResult{
			Status: StatusMissingCredentials,
			Detail: "GCAL_ICS_URL is not set or is empty",
		}
	}
	feedURL := strings.TrimSpace(raw)
	parsed, err := url.Parse(feedURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return collectCalendarResult{
			Status: StatusFailed,
			Detail: "the configured feed URL is not a usable absolute http(s) URL",
		}
	}

	client := &http.Client{Timeout: collectHTTPTimeout}
	resp, err := client.Get(feedURL)
	if err != nil {
		// Sanitized: network error text can embed the requested URL.
		return collectCalendarResult{
			Status: StatusFailed,
			Detail: "the configured feed could not be fetched",
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return collectCalendarResult{
			Status: StatusFailed,
			Detail: fmt.Sprintf("the configured feed responded with HTTP status %d", resp.StatusCode),
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxICSBodyBytes))
	if err != nil {
		return collectCalendarResult{Status: StatusFailed, Detail: "the configured feed could not be read"}
	}
	cal, err := ics.ParseCalendar(strings.NewReader(string(body)))
	if err != nil {
		return collectCalendarResult{Status: StatusFailed, Detail: "the configured feed did not parse as iCalendar data"}
	}

	meetings, issues := expandFeed(cal, calUser, windowStart, windowEnd, dayLoc)
	status, detail := StatusComplete, ""
	if len(body) == maxICSBodyBytes {
		issues = append(issues, "the feed reached the collector's read-size limit before ending")
	}
	if len(issues) > 0 {
		status = StatusIncomplete
		detail = fmt.Sprintf("%d feed entry or expansion issue(s) made coverage incomplete", len(issues))
	}
	if len(meetings) >= maxEvidenceMeetings {
		meetings = meetings[:maxEvidenceMeetings]
		status = StatusIncomplete
		detail = fmt.Sprintf("the feed contained more than the %d-meeting evidence cap", maxEvidenceMeetings)
	}
	return collectCalendarResult{Status: status, Detail: detail, Meetings: meetings}
}

type icsEvent struct {
	UID         string
	StartValue  string
	StartTZID   string
	AllDay      bool
	EndValue    string
	EndTZID     string
	EndSet      bool
	Duration    string
	Status      string
	Summary     string
	RRULE       string
	ExDates     []icsDateTimeProp
	RDates      []icsDateTimeProp
	RecIDValue  string
	RecIDTZID   string
	RecIDAllday bool
	Attendees   []calendarAttendee
}

// icsDateTimeProp preserves one recurring-set property (EXDATE/RDATE) with
// the parameter metadata the series' own DTSTART metadata cannot substitute
// for: each property's TZID and VALUE form govern how its values are
// interpreted (F5). Comma-separated values within one property share that
// property's parameters per RFC 5545 3.8.5.
type icsDateTimeProp struct {
	Value  string
	TZID   string
	AllDay bool
}

type calendarAttendee struct {
	Email    string
	PartStat string
}

// scanEvents reads the raw IANAToken names directly so the implementation does
// not depend on enum indirection inside the pinned parser.
func scanEvents(cal *ics.Calendar) []icsEvent {
	var out []icsEvent
	for _, ev := range cal.Events() {
		e := icsEvent{}
		for _, p := range ev.Properties {
			switch icsToken(p) {
			case "UID":
				e.UID = strings.TrimSpace(p.Value)
			case "DTSTART":
				e.StartValue = strings.TrimSpace(p.Value)
				e.StartTZID = icsParamValue(p, "TZID")
				e.AllDay = icsIsDateForm(p)
			case "DTEND":
				e.EndValue = strings.TrimSpace(p.Value)
				e.EndTZID = icsParamValue(p, "TZID")
				e.EndSet = true
			case "DURATION":
				e.Duration = strings.TrimSpace(p.Value)
			case "STATUS":
				e.Status = strings.ToUpper(strings.TrimSpace(p.Value))
			case "SUMMARY":
				e.Summary = strings.TrimSpace(p.Value)
			case "RRULE":
				e.RRULE = strings.TrimSpace(p.Value)
			case "EXDATE":
				e.ExDates = append(e.ExDates, icsDateTimeProp{
					Value:  strings.TrimSpace(p.Value),
					TZID:   icsParamValue(p, "TZID"),
					AllDay: icsIsDateForm(p),
				})
			case "RDATE":
				e.RDates = append(e.RDates, icsDateTimeProp{
					Value:  strings.TrimSpace(p.Value),
					TZID:   icsParamValue(p, "TZID"),
					AllDay: icsIsDateForm(p),
				})
			case "RECURRENCE-ID":
				e.RecIDValue = strings.TrimSpace(p.Value)
				e.RecIDTZID = icsParamValue(p, "TZID")
				e.RecIDAllday = icsIsDateForm(p)
			case "ATTENDEE":
				e.Attendees = append(e.Attendees, calendarAttendee{
					Email:    calendarAttendeeEmail(p.Value),
					PartStat: icsParam(p, "PARTSTAT"),
				})
			}
		}
		out = append(out, e)
	}
	return out
}

func icsToken(p ics.IANAProperty) string { return strings.ToUpper(strings.TrimSpace(p.IANAToken)) }

func icsParam(p ics.IANAProperty, name string) string {
	if vals, ok := p.ICalParameters[name]; ok && len(vals) > 0 {
		return strings.ToUpper(strings.TrimSpace(vals[0]))
	}
	return ""
}

// icsParamValue returns a parameter value with its original case preserved
// (TZID and other case-sensitive values; only parameter NAMES are
// case-insensitive in RFC 5545).
func icsParamValue(p ics.IANAProperty, name string) string {
	for key, vals := range p.ICalParameters {
		if !strings.EqualFold(key, name) {
			continue
		}
		if len(vals) > 0 {
			return strings.TrimSpace(vals[0])
		}
	}
	return ""
}

func icsIsDateForm(p ics.IANAProperty) bool {
	if vals, ok := p.ICalParameters["VALUE"]; ok {
		for _, v := range vals {
			if strings.EqualFold(strings.TrimSpace(v), "DATE") {
				return true
			}
		}
	}
	return len(p.Value) == 8 && icsParsesDate(p.Value)
}

func icsParsesDate(v string) bool {
	_, err := time.Parse("20060102", v)
	return err == nil
}

func calendarAttendeeEmail(value string) string {
	v := strings.TrimSpace(value)
	v = strings.TrimPrefix(v, "mailto:")
	v = strings.TrimPrefix(v, "MAILTO:")
	return strings.TrimSpace(v)
}

func parseICSDate(value, tzid string, allDay bool, fallbackLoc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if allDay {
		d, err := time.ParseInLocation("20060102", value, fallbackLoc)
		if err != nil {
			return time.Time{}, fmt.Errorf("unsupported date form")
		}
		return d, nil
	}
	if len(value) != 15 && len(value) != 16 {
		return time.Time{}, fmt.Errorf("unsupported datetime form")
	}
	base := value[:15]
	loc := fallbackLoc
	switch {
	case len(value) == 16 && (value[15] == 'Z' || value[15] == 'z'):
		loc = time.UTC
	case tzid != "":
		loaded, err := time.LoadLocation(tzid)
		if err != nil {
			return time.Time{}, fmt.Errorf("event timezone is not a resolvable IANA name")
		}
		loc = loaded
	}
	t, err := time.ParseInLocation("20060102T150405", base, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("unsupported datetime form")
	}
	return t, nil
}

// parseICSDuration parses RFC 5545 DURATION values ([-]PnWnDTnHnMnS).
func parseICSDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToUpper(v))
	if len(v) < 2 || v[0] != 'P' {
		return 0, fmt.Errorf("unsupported duration form")
	}
	neg := false
	v = v[1:]
	if v[0] == '-' || v[0] == '+' {
		neg = v[0] == '-'
		v = v[1:]
	}
	readInt := func(s string) (int, string, bool) {
		num, i := 0, 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			num = num*10 + int(s[i]-'0')
			i++
		}
		if i == 0 {
			return 0, s, false
		}
		return num, s[i:], true
	}
	var weeks, days, hours, mins, secs int
	inTime := false
	rest := v
	for len(rest) > 0 {
		if rest[0] == 'T' {
			inTime, rest = true, rest[1:]
			continue
		}
		num, tail, ok := readInt(rest)
		if !ok || len(tail) == 0 {
			return 0, fmt.Errorf("unsupported duration form")
		}
		unit := tail[0]
		tail = tail[1:]
		switch unit {
		case 'W':
			if inTime {
				return 0, fmt.Errorf("unsupported duration form")
			}
			weeks = num
		case 'D':
			if inTime {
				return 0, fmt.Errorf("unsupported duration form")
			}
			days = num
		case 'H':
			if !inTime {
				return 0, fmt.Errorf("unsupported duration form")
			}
			hours = num
		case 'M':
			if !inTime {
				return 0, fmt.Errorf("unsupported duration form")
			}
			mins = num
		case 'S':
			if !inTime {
				return 0, fmt.Errorf("unsupported duration form")
			}
			secs = num
		default:
			return 0, fmt.Errorf("unsupported duration form")
		}
		rest = tail
	}
	d := time.Duration(weeks)*7*24*time.Hour + time.Duration(days)*24*time.Hour +
		time.Duration(hours)*time.Hour + time.Duration(mins)*time.Minute +
		time.Duration(secs)*time.Second
	if neg {
		d = -d
	}
	return d, nil
}

type icsGroup struct {
	uid         string
	master      *icsEvent
	overrides   []*icsEvent
	overrideIDs map[string]bool
	order       int
}

// expandFeed resolves every parsed VEVENT into proposed meetings overlapping
// the window. Events sharing a UID form one series; overridden instances
// replace the master's matching occurrence, whether cancelled or rescheduled.
func expandFeed(cal *ics.Calendar, calUser string, windowStart, windowEnd time.Time, dayLoc *time.Location) ([]ProposedMeeting, []string) {
	events := scanEvents(cal)
	var issues []string

	groups := map[string]*icsGroup{}
	var order []string
	for i := range events {
		e := &events[i]
		if e.UID == "" {
			e.UID = fmt.Sprintf("cal-unnamed-%d", i)
			issues = append(issues, "an event without a UID was given a synthetic identity and kept")
		}
		if strings.ContainsFunc(e.UID, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			issues = append(issues, "an event UID contained control characters and its entry was skipped")
			continue
		}
		g, ok := groups[e.UID]
		if !ok {
			g = &icsGroup{uid: e.UID, order: len(order), overrideIDs: map[string]bool{}}
			groups[e.UID] = g
			order = append(order, e.UID)
		}
		if e.RecIDValue != "" {
			g.overrides = append(g.overrides, e)
		} else {
			g.master = e
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return groups[order[i]].order < groups[order[j]].order })

	var meetings []ProposedMeeting
	for _, uid := range order {
		g := groups[uid]
		for _, ov := range g.overrides {
			if ov.RecIDAllday {
				d := startDateOnly(ov.RecIDValue)
				if _, err := allDayInstant(d, dayLoc); err != nil {
					issues = append(issues, "an overridden event's RECURRENCE-ID date does not exist in the recorded day's timezone; its series may keep the replaced instance")
					continue
				}
				g.overrideIDs[allDayKey(d)] = true
				continue
			}
			recidStart, err := parseICSDate(ov.RecIDValue, ov.RecIDTZID, ov.RecIDAllday, dayLoc)
			if err != nil {
				issues = append(issues, "an overridden event's RECURRENCE-ID could not be resolved; its series may keep the replaced instance")
				continue
			}
			g.overrideIDs[instKey(recidStart)] = true
		}
		if g.master != nil {
			ms, iss := expandMaster(g, windowStart, windowEnd, dayLoc, calUser)
			meetings = append(meetings, ms...)
			issues = append(issues, iss...)
		}
		for _, ov := range g.overrides {
			m, kept, iss := overrideMeeting(ov, g.uid, windowStart, windowEnd, dayLoc, calUser)
			if iss != "" {
				issues = append(issues, iss)
			}
			if kept {
				meetings = append(meetings, m)
			}
		}
	}
	sort.SliceStable(meetings, func(i, j int) bool {
		if meetings[i].Start != meetings[j].Start {
			return meetings[i].Start < meetings[j].Start
		}
		return meetings[i].Ref < meetings[j].Ref
	})
	return meetings, issues
}

func expandMaster(g *icsGroup, windowStart, windowEnd time.Time, dayLoc *time.Location, calUser string) ([]ProposedMeeting, []string) {
	master := g.master
	var issues []string

	start, err := parseICSDate(master.StartValue, master.StartTZID, master.AllDay, dayLoc)
	if err != nil {
		issues = append(issues, "an event's DTSTART could not be resolved; it was not collected")
		return nil, issues
	}

	var meetings []ProposedMeeting
	addInstance := func(start, end time.Time, allDay bool, displayStart, displayEnd, recKind string) {
		m, ok, iss := buildMeeting(master, g.uid, start, end, allDay, displayStart, displayEnd, recKind, calUser, windowStart, windowEnd)
		if iss != "" {
			issues = append(issues, iss)
		}
		if ok {
			meetings = append(meetings, m)
		}
	}

	if master.AllDay {
		startDateText := startDateOnly(master.StartValue)
		if _, err := allDayInstant(startDateText, dayLoc); err != nil {
			issues = append(issues, "an all-day event's DTSTART date does not exist in the recorded day's timezone; it was not collected")
			return nil, issues
		}
		spanDays := 1
		if master.EndSet {
			endDateText := startDateOnly(master.EndValue)
			if _, err := parseICSDate(master.EndValue, master.EndTZID, true, dayLoc); err != nil {
				issues = append(issues, "an all-day event's DTEND could not be resolved; its coverage fell back to one day")
			} else if days := daysBetweenDates(startDateText, endDateText); days <= 0 {
				issues = append(issues, "an all-day event's DTEND did not extend beyond its DTSTART; its coverage fell back to one day")
			} else {
				spanDays = days
			}
		}

		rdateDates, iss := allDayDates(master.RDates, dayLoc, "RDATE")
		issues = append(issues, iss...)
		exdateDates, iss := allDayDates(master.ExDates, dayLoc, "EXDATE")
		issues = append(issues, iss...)
		excluded := map[string]bool{}
		for _, d := range exdateDates {
			excluded[d] = true
		}

		if master.RRULE == "" {
			for _, d := range dedupDates(append([]string{startDateText}, rdateDates...)) {
				if excluded[d] || g.overrideIDs[allDayKey(d)] {
					continue
				}
				if iss := addAllDayInstance(addInstance, d, spanDays, dayLoc, "single"); iss != "" {
					issues = append(issues, iss)
				}
			}
			return meetings, issues
		}
		set, err := buildAllDayRRuleSet(master, startDateText, rdateDates, exdateDates)
		if err != nil {
			issues = append(issues, "a recurring all-day event's rule could not be expanded; it was not collected")
			return meetings, issues
		}
		fromDate := addDateText(windowStart.In(dayLoc).Format("2006-01-02"), -(spanDays + 1))
		toDate := windowEnd.In(dayLoc).Format("2006-01-02")
		fromInst, err := civilInstant(fromDate)
		if err != nil {
			issues = append(issues, "a recurring all-day event's expansion window could not be resolved; it was not collected")
			return meetings, issues
		}
		toInst, err := civilInstant(toDate)
		if err != nil {
			issues = append(issues, "a recurring all-day event's expansion window could not be resolved; it was not collected")
			return meetings, issues
		}
		occurrences := set.Between(fromInst, toInst, false)
		count := 0
		for _, occ := range occurrences {
			occDate := occ.Format("2006-01-02")
			if excluded[occDate] || g.overrideIDs[allDayKey(occDate)] {
				continue
			}
			if count >= 500 {
				issues = append(issues, "a recurring all-day event produced more than the bounded expansion count")
				break
			}
			count++
			if iss := addAllDayInstance(addInstance, occDate, spanDays, dayLoc, "recurring"); iss != "" {
				issues = append(issues, iss)
			}
		}
		return meetings, issues
	}

	end, hasEnd, endErr := resolveEventEnd(master, start, dayLoc)
	if endErr != nil {
		issues = append(issues, "an event's DTEND/DURATION could not be resolved; it was not collected")
		return nil, issues
	}
	_ = hasEnd

	rdates, iss := seriesDateTimes(master.RDates, dayLoc, "RDATE")
	issues = append(issues, iss...)
	exdates, iss := seriesDateTimes(master.ExDates, dayLoc, "EXDATE")
	issues = append(issues, iss...)

	excluded := map[string]bool{}
	for _, t := range exdates {
		excluded[instKey(t)] = true
	}

	if master.RRULE == "" {
		occStarts := dedupInstances(append([]time.Time{start}, rdates...))
		for _, occ := range occStarts {
			if excluded[instKey(occ)] || g.overrideIDs[instKey(occ)] {
				continue
			}
			instEnd := end.Add(occ.Sub(start))
			addInstance(occ, instEnd, false, icsRender(occ), icsRender(instEnd), "single")
		}
		return meetings, issues
	}

	set, err := buildRRuleSet(master, start, rdates, exdates)
	if err != nil {
		issues = append(issues, "a recurring event's rule could not be expanded; it was not collected")
		return meetings, issues
	}
	lookback := end.Sub(start)
	if lookback < 0 {
		lookback = 0
	}
	occurrences := set.Between(windowStart.Add(-lookback-24*time.Hour), windowEnd, false)
	count := 0
	for _, occ := range occurrences {
		if excluded[instKey(occ)] || g.overrideIDs[instKey(occ)] {
			continue
		}
		if count >= 500 {
			issues = append(issues, "a recurring event produced more than the bounded expansion count")
			break
		}
		count++
		if !occ.Before(windowEnd) {
			continue
		}
		instEnd := end.Add(occ.Sub(start))
		if !instEnd.After(windowStart) {
			continue
		}
		addInstance(occ, instEnd, false, icsRender(occ), icsRender(instEnd), "recurring")
	}
	return meetings, issues
}

// addAllDayInstance renders one all-day occurrence from its declared civil
// date and the series' calendrical span. The occurrence instant and the
// exclusive end instant are both resolved from their own dates with the
// first-real-instant rule (F4), so adding the span never carries a nonzero
// start clock into the exclusive end's date, and no hours are invented for a
// date-only event.
func addAllDayInstance(add func(start, end time.Time, allDay bool, displayStart, displayEnd, recKind string), dateText string, spanDays int, dayLoc *time.Location, recKind string) string {
	start, err := allDayInstant(dateText, dayLoc)
	if err != nil {
		return "an all-day occurrence's date does not exist in the recorded day's timezone; it was not collected"
	}
	endText := addDateText(dateText, spanDays)
	end, err := allDayEndInstant(endText, dayLoc)
	if err != nil {
		return "an all-day occurrence's exclusive end date could not be resolved; it was not collected"
	}
	add(start, end, true, dateText, endText, recKind)
	return ""
}

func seriesDateTimes(props []icsDateTimeProp, dayLoc *time.Location, name string) ([]time.Time, []string) {
	var out []time.Time
	var issues []string
	for _, prop := range props {
		for _, item := range strings.Split(prop.Value, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if prop.AllDay {
				issues = append(issues, fmt.Sprintf(
					"a %s value form does not match its series' DATE DTSTART form; it was not applied and the series is not complete coverage", name))
				continue
			}
			t, err := parseICSDate(item, prop.TZID, prop.AllDay, dayLoc)
			if err != nil {
				issues = append(issues, fmt.Sprintf(
					"a %s value could not be resolved in its own timezone; it was not applied and the series is not complete coverage", name))
				continue
			}
			out = append(out, t)
		}
	}
	return out, issues
}

func allDayDates(props []icsDateTimeProp, dayLoc *time.Location, name string) ([]string, []string) {
	var out []string
	var issues []string
	for _, prop := range props {
		for _, item := range strings.Split(prop.Value, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if !prop.AllDay {
				issues = append(issues, fmt.Sprintf(
					"a %s value form does not match its series' DATE DTSTART form; it was not applied and the series is not complete coverage", name))
				continue
			}
			d := startDateOnly(item)
			if _, err := allDayInstant(d, dayLoc); err != nil {
				issues = append(issues, fmt.Sprintf(
					"a %s value could not be resolved as a civil date in the recorded day's timezone; it was not applied and the series is not complete coverage", name))
				continue
			}
			out = append(out, d)
		}
	}
	return out, issues
}

func buildRRuleSet(master *icsEvent, start time.Time, rdates, exdates []time.Time) (*rrule.Set, error) {
	option, err := rrule.StrToROptionInLocation(master.RRULE, start.Location())
	if err != nil {
		return nil, err
	}
	option.Dtstart = start
	rule, err := rrule.NewRRule(*option)
	if err != nil {
		return nil, err
	}
	set := &rrule.Set{}
	set.RRule(rule)
	set.DTStart(start)
	for _, t := range exdates {
		set.ExDate(t)
	}
	for _, t := range rdates {
		set.RDate(t)
	}
	return set, nil
}

// buildAllDayRRuleSet expands a DATE series on civil dates: DTSTART, RDATE and
// EXDATE are canonical UTC midnights for the declared dates, so recurrence
// arithmetic can neither move an occurrence across a skipped local midnight
// nor invent a 23/25-hour day. Each resulting date is resolved to the zone's
// first real instant before overlap is tested (F4/F5).
func buildAllDayRRuleSet(master *icsEvent, startDate string, rdateDates, exdateDates []string) (*rrule.Set, error) {
	start, err := civilInstant(startDate)
	if err != nil {
		return nil, err
	}
	rdates := make([]time.Time, 0, len(rdateDates))
	for _, d := range rdateDates {
		t, err := civilInstant(d)
		if err != nil {
			return nil, err
		}
		rdates = append(rdates, t)
	}
	exdates := make([]time.Time, 0, len(exdateDates))
	for _, d := range exdateDates {
		t, err := civilInstant(d)
		if err != nil {
			return nil, err
		}
		exdates = append(exdates, t)
	}
	return buildRRuleSet(master, start, rdates, exdates)
}

func dedupInstances(in []time.Time) []time.Time {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]time.Time, 0, len(in))
	for _, t := range in {
		k := instKey(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	return out
}

func dedupDates(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

func daysBetweenDates(from, to string) int {
	a, err1 := time.Parse("2006-01-02", from)
	b, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil {
		return -1
	}
	return int(b.Sub(a) / (24 * time.Hour))
}

func addDateText(d string, days int) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.AddDate(0, 0, days).Format("2006-01-02")
}

func instKey(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func startDateOnly(v string) string {
	return v[:4] + "-" + v[4:6] + "-" + v[6:8]
}

func nextDateString(d string) string {
	return addDateText(d, 1)
}

func allDayKey(d string) string { return "d:" + d }

// allDayInstant resolves a civil DATE to that date's first real instant in the
// recorded day's timezone. It is the same skipped/repeated-midnight rule the
// day-window resolver uses, so a DATE event keeps its declared date even when
// local midnight is skipped or repeated (F4).
func allDayInstant(dateText string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		return time.Time{}, fmt.Errorf("recorded day timezone is unavailable")
	}
	return localDayStart(dateText, loc)
}

// allDayEndInstant resolves an exclusive DATE boundary to an instant. Normally
// that is the date's first real instant; if the whole civil date is absent in
// the zone, the first instant of the next existing date is the same boundary.
func allDayEndInstant(dateText string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		return time.Time{}, fmt.Errorf("recorded day timezone is unavailable")
	}
	for i := 0; i <= 3; i++ {
		d := dateText
		if i > 0 {
			d = addDateText(dateText, i)
		}
		if t, err := localDayStart(d, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("exclusive end date could not be resolved in the recorded day's timezone")
}

// civilInstant is the canonical UTC midnight of a civil date. Date-only
// recurrence expands on these values so a zone transition can never shift an
// occurrence's date.
func civilInstant(dateText string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", dateText)
	if err != nil {
		return time.Time{}, fmt.Errorf("date form is unsupported")
	}
	return t, nil
}

func icsRender(t time.Time) string { return t.Format(time.RFC3339) }

// resolveEventEnd computes the event end instant: DTEND wins, else DURATION
// from DTSTART, else the start instant (a zero-length instant event).
func resolveEventEnd(master *icsEvent, start time.Time, dayLoc *time.Location) (time.Time, bool, error) {
	if master.EndSet {
		end, err := parseICSDate(master.EndValue, master.EndTZID, false, dayLoc)
		if err != nil {
			return time.Time{}, false, err
		}
		return end, true, nil
	}
	if master.Duration != "" {
		d, err := parseICSDuration(master.Duration)
		if err != nil {
			return time.Time{}, false, err
		}
		return start.Add(d), true, nil
	}
	return start, false, nil
}

func buildMeeting(master *icsEvent, uid string, start, end time.Time, allDay bool, displayStart, displayEnd, recKind string, calUser string, windowStart, windowEnd time.Time) (ProposedMeeting, bool, string) {
	status := "scheduled"
	if strings.EqualFold(master.Status, "CANCELLED") {
		status = "cancelled"
	}
	attendance := "unknown"
	if strings.TrimSpace(calUser) != "" {
		for _, at := range master.Attendees {
			if strings.EqualFold(at.Email, calUser) {
				attendance = normalPartStat(at.PartStat)
				break
			}
		}
	}
	overlaps := start.Before(windowEnd) && end.After(windowStart)
	if !overlaps {
		return ProposedMeeting{}, false, ""
	}
	refSuffix := start.UTC().Format("20060102T150405Z")
	if allDay {
		refSuffix = displayStart
	}
	return ProposedMeeting{
		Ref:        "cal:" + uid + "@" + refSuffix,
		UID:        uid,
		Title:      sanitizerSummaryText(master.Summary),
		Start:      displayStart,
		End:        displayEnd,
		AllDay:     allDay,
		Status:     status,
		Attendance: attendance,
		Recurrence: recKind,
	}, true, ""
}

func normalPartStat(v string) string {
	s := strings.ToUpper(strings.TrimSpace(v))
	switch {
	case strings.HasPrefix(s, "ACCEPTED"):
		return "accepted"
	case strings.HasPrefix(s, "DECLINED"):
		return "declined"
	case strings.HasPrefix(s, "TENTATIVE"):
		return "tentative"
	case strings.HasPrefix(s, "DELEGATED"):
		return "delegated"
	case strings.HasPrefix(s, "NEEDS-ACTION"):
		return "needs_action"
	default:
		return "unknown"
	}
}

func sanitizerSummaryText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return truncateRunes(strings.TrimSpace(s), maxTitleRunes)
}

func overrideMeeting(ov *icsEvent, uid string, windowStart, windowEnd time.Time, dayLoc *time.Location, calUser string) (ProposedMeeting, bool, string) {
	status := "scheduled"
	if strings.EqualFold(ov.Status, "CANCELLED") {
		status = "cancelled"
	}
	attendance := "unknown"
	if strings.TrimSpace(calUser) != "" {
		for _, at := range ov.Attendees {
			if strings.EqualFold(at.Email, calUser) {
				attendance = normalPartStat(at.PartStat)
				break
			}
		}
	}

	var displayStart, displayEnd, ref string
	var overlaps bool
	issueText := ""
	if ov.AllDay {
		s := startDateOnly(ov.StartValue)
		startInst, err := allDayInstant(s, dayLoc)
		if err != nil {
			return ProposedMeeting{}, false, "an all-day override's DTSTART date does not exist in the recorded day's timezone; it was not collected"
		}
		e := nextDateString(s)
		if ov.EndSet {
			if _, err := parseICSDate(ov.EndValue, ov.EndTZID, true, dayLoc); err != nil {
				issueText = "an all-day override's DTEND could not be resolved; its coverage fell back to one day"
			} else if cand := startDateOnly(ov.EndValue); daysBetweenDates(s, cand) <= 0 {
				issueText = "an all-day override's DTEND did not extend beyond its DTSTART; its coverage fell back to one day"
			} else {
				e = cand
			}
		}
		endInst, err := allDayEndInstant(e, dayLoc)
		if err != nil {
			return ProposedMeeting{}, false, "an all-day override's exclusive end date could not be resolved; it was not collected"
		}
		overlaps = startInst.Before(windowEnd) && endInst.After(windowStart)
		recidDate := startDateOnly(ov.RecIDValue)
		if status == "cancelled" {
			recidStart, rErr := allDayInstant(recidDate, dayLoc)
			recidEnd, rEndErr := allDayEndInstant(nextDateString(recidDate), dayLoc)
			if rErr == nil && rEndErr == nil {
				overlaps = overlaps || (recidStart.Before(windowEnd) && recidEnd.After(windowStart))
			}
		}
		displayStart, displayEnd = s, e
		ref = "cal:" + uid + "@" + recidDate + "->" + s + "-override"
	} else {
		recidStart, err := parseICSDate(ov.RecIDValue, ov.RecIDTZID, ov.RecIDAllday, dayLoc)
		if err != nil {
			return ProposedMeeting{}, false, "an overridden event's RECURRENCE-ID could not be resolved; it was not collected"
		}
		start, err := parseICSDate(ov.StartValue, ov.StartTZID, ov.AllDay, dayLoc)
		if err != nil {
			return ProposedMeeting{}, false, "an overridden event's DTSTART could not be resolved; it was not collected"
		}
		end, _, endErr := resolveEventEnd(ov, start, dayLoc)
		if endErr != nil {
			return ProposedMeeting{}, false, "an overridden event's DTEND/DURATION could not be resolved; it was not collected"
		}
		if !end.After(start) {
			end = start
		}
		overlaps = start.Before(windowEnd) && end.After(windowStart)
		if status == "cancelled" {
			recidOverlaps := !recidStart.Before(windowStart) && recidStart.Before(windowEnd)
			overlaps = overlaps || recidOverlaps
		}
		displayStart, displayEnd = icsRender(start), icsRender(end)
		ref = "cal:" + uid + "@" + recidStart.UTC().Format("20060102T150405Z") + "->" + start.UTC().Format("20060102T150405Z") + "-override"
	}
	if !overlaps {
		return ProposedMeeting{}, false, ""
	}
	return ProposedMeeting{
		Ref:        ref,
		UID:        uid,
		Title:      sanitizerSummaryText(ov.Summary),
		Start:      displayStart,
		End:        displayEnd,
		AllDay:     ov.AllDay,
		Status:     status,
		Attendance: attendance,
		Recurrence: "override",
	}, true, issueText
}

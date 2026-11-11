package timesheet

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const calendarTestUser = "me@example.invalid"

func collectCalendarDay(t *testing.T, timezone, date, events string) collectCalendarResult {
	t.Helper()
	feed := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//synthetic//EN\r\n" +
		strings.ReplaceAll(strings.TrimSpace(events), "\n", "\r\n") + "\r\nEND:VCALENDAR\r\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(feed))
	}))
	defer srv.Close()
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatal(err)
	}
	start, err := localDayStart(date, loc)
	if err != nil {
		t.Fatal(err)
	}
	end, err := localDayStart(nextDate(date), loc)
	if err != nil {
		t.Fatal(err)
	}
	return collectCalendar(calendarTestUser, map[string]string{"GCAL_ICS_URL": srv.URL}, start, end, loc)
}

func vevent(lines ...string) string {
	return "BEGIN:VEVENT\n" + strings.Join(lines, "\n") + "\nEND:VEVENT\n"
}

func TestCalendarExpansion(t *testing.T) {
	const ams = "Europe/Amsterdam"
	weekly := []string{"UID:w", "SUMMARY:Weekly", "DTSTART;TZID=Europe/Amsterdam:20261015T090000",
		"DTEND;TZID=Europe/Amsterdam:20261015T100000", "RRULE:FREQ=WEEKLY"}
	allDayWeekly := []string{"UID:aw", "SUMMARY:Offsite", "DTSTART;VALUE=DATE:20261015", "RRULE:FREQ=WEEKLY"}
	with := func(base []string, extra ...string) []string { return append(append([]string(nil), base...), extra...) }

	cases := []struct {
		name     string
		timezone string
		date     string
		events   string
		status   string
		want     []string // "ref start end status attendance recurrence"
	}{
		{
			name: "a TZID start keeps its own zone and offset", timezone: ams, date: "2026-10-22",
			events: vevent("UID:a", "DTSTART;TZID=America/New_York:20261022T090000", "DTEND;TZID=America/New_York:20261022T100000"),
			status: StatusComplete,
			want:   []string{"cal:a@20261022T130000Z 2026-10-22T09:00:00-04:00 2026-10-22T10:00:00-04:00 scheduled unknown single"},
		},
		{
			name: "a floating start is anchored to the recorded day's timezone", timezone: ams, date: "2026-10-22",
			events: vevent("UID:f", "DTSTART:20261022T090000", "DTEND:20261022T100000"),
			status: StatusComplete,
			want:   []string{"cal:f@20261022T070000Z 2026-10-22T09:00:00+02:00 2026-10-22T10:00:00+02:00 scheduled unknown single"},
		},
		{
			name: "a UTC start keeps its exact instant", timezone: ams, date: "2026-10-22",
			events: vevent("UID:z", "DTSTART:20261022T070000Z", "DTEND:20261022T080000Z"),
			status: StatusComplete,
			want:   []string{"cal:z@20261022T070000Z 2026-10-22T07:00:00Z 2026-10-22T08:00:00Z scheduled unknown single"},
		},
		{
			name: "an unknown TZID is an incomplete-coverage issue", timezone: ams, date: "2026-10-22",
			events: vevent("UID:x", "DTSTART;TZID=Mars/Olympus:20261022T090000", "DTEND;TZID=Mars/Olympus:20261022T100000"),
			status: StatusIncomplete,
		},
		{
			name: "membership is half-open interval overlap with the day window", timezone: ams, date: "2026-10-22",
			events: vevent("UID:before", "DTSTART:20261021T230000", "DTEND:20261022T000000") +
				vevent("UID:across-start", "DTSTART:20261021T233000", "DTEND:20261022T003000") +
				vevent("UID:across-end", "DTSTART:20261022T233000", "DTEND:20261023T003000") +
				vevent("UID:after", "DTSTART:20261023T000000", "DTEND:20261023T010000"),
			status: StatusComplete,
			want: []string{
				"cal:across-start@20261021T213000Z 2026-10-21T23:30:00+02:00 2026-10-22T00:30:00+02:00 scheduled unknown single",
				"cal:across-end@20261022T213000Z 2026-10-22T23:30:00+02:00 2026-10-23T00:30:00+02:00 scheduled unknown single",
			},
		},
		{
			name: "DURATION gives the end when DTEND is absent", timezone: ams, date: "2026-10-22",
			events: vevent("UID:d", "DTSTART:20261022T090000", "DURATION:PT1H30M"),
			status: StatusComplete,
			want:   []string{"cal:d@20261022T070000Z 2026-10-22T09:00:00+02:00 2026-10-22T10:30:00+02:00 scheduled unknown single"},
		},
		{
			name: "a weekly series keeps its wall clock across a DST change", timezone: ams, date: "2026-10-29",
			events: vevent(weekly...),
			status: StatusComplete,
			want:   []string{"cal:w@20261029T080000Z 2026-10-29T09:00:00+01:00 2026-10-29T10:00:00+01:00 scheduled unknown recurring"},
		},
		{
			name: "an EXDATE is resolved in its own timezone", timezone: ams, date: "2026-10-22",
			events: vevent(with(weekly, "EXDATE;TZID=America/New_York:20261022T030000")...),
			status: StatusComplete,
		},
		{
			name: "an RDATE adds an instance with the series' duration", timezone: ams, date: "2026-10-22",
			events: vevent("UID:r", "DTSTART;TZID=Europe/Amsterdam:20261015T090000", "DTEND;TZID=Europe/Amsterdam:20261015T100000",
				"RDATE;TZID=Europe/Amsterdam:20261022T140000"),
			status: StatusComplete,
			want:   []string{"cal:r@20261022T120000Z 2026-10-22T14:00:00+02:00 2026-10-22T15:00:00+02:00 scheduled unknown single"},
		},
		{
			name: "an RDATE adds an instance to a recurring series", timezone: ams, date: "2026-10-22",
			events: vevent(with(weekly, "RDATE;TZID=Europe/Amsterdam:20261022T140000")...),
			status: StatusComplete,
			want: []string{
				"cal:w@20261022T070000Z 2026-10-22T09:00:00+02:00 2026-10-22T10:00:00+02:00 scheduled unknown recurring",
				"cal:w@20261022T120000Z 2026-10-22T14:00:00+02:00 2026-10-22T15:00:00+02:00 scheduled unknown recurring",
			},
		},
		{
			name: "an EXDATE removes an RDATE instance", timezone: ams, date: "2026-10-22",
			events: vevent("UID:r", "DTSTART;TZID=Europe/Amsterdam:20261015T090000", "DTEND;TZID=Europe/Amsterdam:20261015T100000",
				"RDATE;TZID=Europe/Amsterdam:20261022T140000", "EXDATE:20261022T120000Z"),
			status: StatusComplete,
		},
		{
			name: "an all-day EXDATE removes an all-day RDATE", timezone: ams, date: "2026-10-22",
			events: vevent("UID:ar", "DTSTART;VALUE=DATE:20261015", "RDATE;VALUE=DATE:20261022", "EXDATE;VALUE=DATE:20261022"),
			status: StatusComplete,
		},
		{
			name: "an RDATE equal to an instance is de-duplicated", timezone: ams, date: "2026-10-22",
			events: vevent(with(weekly, "RDATE;TZID=Europe/Amsterdam:20261022T090000")...),
			status: StatusComplete,
			want:   []string{"cal:w@20261022T070000Z 2026-10-22T09:00:00+02:00 2026-10-22T10:00:00+02:00 scheduled unknown recurring"},
		},
		{
			name: "an RDATE equal to a single event's start is de-duplicated", timezone: ams, date: "2026-10-22",
			events: vevent("UID:s", "DTSTART;TZID=Europe/Amsterdam:20261022T090000", "DTEND;TZID=Europe/Amsterdam:20261022T100000",
				"RDATE;TZID=Europe/Amsterdam:20261022T090000"),
			status: StatusComplete,
			want:   []string{"cal:s@20261022T070000Z 2026-10-22T09:00:00+02:00 2026-10-22T10:00:00+02:00 scheduled unknown single"},
		},
		{
			name: "a rescheduled override replaces the occurrence and fingerprints both instants", timezone: ams, date: "2026-10-22",
			events: vevent(weekly...) + vevent("UID:w", "SUMMARY:Moved", "RECURRENCE-ID;TZID=Europe/Amsterdam:20261022T090000",
				"DTSTART;TZID=Europe/Amsterdam:20261022T110000", "DTEND;TZID=Europe/Amsterdam:20261022T120000"),
			status: StatusComplete,
			want:   []string{"cal:w@20261022T070000Z->20261022T090000Z-override 2026-10-22T11:00:00+02:00 2026-10-22T12:00:00+02:00 scheduled unknown override"},
		},
		{
			name: "an override moved to another day leaves its original day empty", timezone: ams, date: "2026-10-22",
			events: vevent(weekly...) + vevent("UID:w", "RECURRENCE-ID;TZID=Europe/Amsterdam:20261022T090000",
				"DTSTART;TZID=Europe/Amsterdam:20261023T090000", "DTEND;TZID=Europe/Amsterdam:20261023T100000"),
			status: StatusComplete,
		},
		{
			name: "a cancelled override still shows on its replaced original's day", timezone: ams, date: "2026-10-22",
			events: vevent(weekly...) + vevent("UID:w", "STATUS:CANCELLED", "RECURRENCE-ID;TZID=Europe/Amsterdam:20261022T090000",
				"DTSTART;TZID=Europe/Amsterdam:20261023T090000", "DTEND;TZID=Europe/Amsterdam:20261023T100000"),
			status: StatusComplete,
			want:   []string{"cal:w@20261022T070000Z->20261023T070000Z-override 2026-10-23T09:00:00+02:00 2026-10-23T10:00:00+02:00 cancelled unknown override"},
		},
		{
			name: "a cancelled override does not show on an unrelated day", timezone: ams, date: "2026-10-21",
			events: vevent(weekly...) + vevent("UID:w", "STATUS:CANCELLED", "RECURRENCE-ID;TZID=Europe/Amsterdam:20261022T090000",
				"DTSTART;TZID=Europe/Amsterdam:20261022T090000", "DTEND;TZID=Europe/Amsterdam:20261022T100000"),
			status: StatusComplete,
		},
		{
			name: "an all-day DTEND is exclusive", timezone: ams, date: "2026-10-23",
			events: vevent("UID:ad", "DTSTART;VALUE=DATE:20261021", "DTEND;VALUE=DATE:20261023"),
			status: StatusComplete,
		},
		{
			name: "an all-day event covers its dates through the day before DTEND", timezone: ams, date: "2026-10-22",
			events: vevent("UID:ad", "DTSTART;VALUE=DATE:20261021", "DTEND;VALUE=DATE:20261023"),
			status: StatusComplete,
			want:   []string{"cal:ad@2026-10-21 2026-10-21 2026-10-23 scheduled unknown single"},
		},
		{
			name: "an all-day event without DTEND covers one date", timezone: ams, date: "2026-10-22",
			events: vevent("UID:one", "DTSTART;VALUE=DATE:20261022") + vevent("UID:prev", "DTSTART;VALUE=DATE:20261021"),
			status: StatusComplete,
			want:   []string{"cal:one@2026-10-22 2026-10-22 2026-10-23 scheduled unknown single"},
		},
		{
			name: "an all-day DTEND not after DTSTART falls back to one date", timezone: ams, date: "2026-10-22",
			events: vevent("UID:bad", "DTSTART;VALUE=DATE:20261022", "DTEND;VALUE=DATE:20261022"),
			status: StatusIncomplete,
			want:   []string{"cal:bad@2026-10-22 2026-10-22 2026-10-23 scheduled unknown single"},
		},
		{
			name: "an interior day of a long recurring all-day instance is covered", timezone: ams, date: "2026-10-21",
			events: vevent("UID:long", "DTSTART;VALUE=DATE:20261019", "DTEND;VALUE=DATE:20261022", "RRULE:FREQ=WEEKLY"),
			status: StatusComplete,
			want:   []string{"cal:long@2026-10-19 2026-10-19 2026-10-22 scheduled unknown recurring"},
		},
		{
			name: "a long recurring timed instance that began earlier is covered", timezone: ams, date: "2026-10-22",
			events: vevent("UID:lt", "DTSTART;TZID=Europe/Amsterdam:20261013T120000", "DTEND;TZID=Europe/Amsterdam:20261015T120000",
				"RRULE:FREQ=WEEKLY"),
			status: StatusComplete,
			want:   []string{"cal:lt@20261020T100000Z 2026-10-20T12:00:00+02:00 2026-10-22T12:00:00+02:00 scheduled unknown recurring"},
		},
		{
			name: "a date-only RDATE on a timed series is reported, not applied", timezone: ams, date: "2026-10-22",
			events: vevent("UID:s", "DTSTART;TZID=Europe/Amsterdam:20261022T090000", "DTEND;TZID=Europe/Amsterdam:20261022T100000",
				"RDATE;VALUE=DATE:20261022"),
			status: StatusIncomplete,
			want:   []string{"cal:s@20261022T070000Z 2026-10-22T09:00:00+02:00 2026-10-22T10:00:00+02:00 scheduled unknown single"},
		},
		{
			name: "a timed EXDATE on an all-day series is reported, not applied", timezone: ams, date: "2026-10-22",
			events: vevent(with(allDayWeekly, "EXDATE;TZID=Europe/Amsterdam:20261022T000000")...),
			status: StatusIncomplete,
			want:   []string{"cal:aw@2026-10-22 2026-10-22 2026-10-23 scheduled unknown recurring"},
		},
		{
			name: "an all-day EXDATE removes its civil date", timezone: ams, date: "2026-10-22",
			events: vevent(with(allDayWeekly, "EXDATE;VALUE=DATE:20261022")...),
			status: StatusComplete,
		},
		{
			name: "a date-only override replaces the occurrence on its declared date", timezone: ams, date: "2026-10-22",
			events: vevent(allDayWeekly...) + vevent("UID:aw", "RECURRENCE-ID;VALUE=DATE:20261022", "DTSTART;VALUE=DATE:20261023"),
			status: StatusComplete,
		},
		{
			name: "a date-only override renders on its new date", timezone: ams, date: "2026-10-23",
			events: vevent(allDayWeekly...) + vevent("UID:aw", "RECURRENCE-ID;VALUE=DATE:20261022", "DTSTART;VALUE=DATE:20261023"),
			status: StatusComplete,
			want:   []string{"cal:aw@2026-10-22->2026-10-23-override 2026-10-23 2026-10-24 scheduled unknown override"},
		},
		{
			name: "an all-day event keeps its date when local midnight is skipped", timezone: "America/Santiago", date: "2026-09-06",
			events: vevent("UID:sk", "DTSTART;VALUE=DATE:20260906") + vevent("UID:sk-prev", "DTSTART;VALUE=DATE:20260905"),
			status: StatusComplete,
			want:   []string{"cal:sk@2026-09-06 2026-09-06 2026-09-07 scheduled unknown single"},
		},
		{
			name: "attendance is the configured user's PARTSTAT and a decline is still listed", timezone: ams, date: "2026-10-22",
			events: vevent("UID:p", "DTSTART:20261022T090000", "DTEND:20261022T100000",
				"ATTENDEE;PARTSTAT=ACCEPTED:mailto:someone@example.invalid",
				"ATTENDEE;PARTSTAT=DECLINED:mailto:ME@example.invalid") +
				vevent("UID:c", "STATUS:CANCELLED", "DTSTART:20261022T110000", "DTEND:20261022T120000"),
			status: StatusComplete,
			want: []string{
				"cal:p@20261022T070000Z 2026-10-22T09:00:00+02:00 2026-10-22T10:00:00+02:00 scheduled declined single",
				"cal:c@20261022T090000Z 2026-10-22T11:00:00+02:00 2026-10-22T12:00:00+02:00 cancelled unknown single",
			},
		},
		{
			name: "an event without a UID is kept under a synthetic identity", timezone: ams, date: "2026-10-22",
			events: vevent("DTSTART:20261022T090000", "DTEND:20261022T100000"),
			status: StatusIncomplete,
			want:   []string{"cal:cal-unnamed-0@20261022T070000Z 2026-10-22T09:00:00+02:00 2026-10-22T10:00:00+02:00 scheduled unknown single"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectCalendarDay(t, tc.timezone, tc.date, tc.events)
			var meetings []string
			for _, m := range got.Meetings {
				meetings = append(meetings, strings.Join([]string{m.Ref, m.Start, m.End, m.Status, m.Attendance, m.Recurrence}, " "))
			}
			if got.Status != tc.status || !reflect.DeepEqual(meetings, tc.want) {
				t.Fatalf("status %s (%s), meetings:\n%s\nwant status %s, meetings:\n%s",
					got.Status, got.Detail, strings.Join(meetings, "\n"), tc.status, strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestCalendarSeriesExpansionIsBounded(t *testing.T) {
	got := collectCalendarDay(t, "Europe/Amsterdam", "2026-10-22",
		vevent("UID:m", "DTSTART:20261021T000000", "DURATION:PT1M", "RRULE:FREQ=MINUTELY;INTERVAL=2"))
	if got.Status != StatusIncomplete || !strings.Contains(got.Detail, "expansion issue") || len(got.Meetings) >= maxEvidenceMeetings {
		t.Fatalf("a series beyond the 500-instance expansion bound must stop there and report it: %s (%s), %d meetings",
			got.Status, got.Detail, len(got.Meetings))
	}
}

const feedMarker = "ICSFEEDSECRET302"

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr, origLog := os.Stderr, log.Writer()
	logged := &lockedBuffer{}
	os.Stderr = w
	log.SetOutput(logged)
	read := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		read <- b
	}()
	defer func() {
		os.Stderr = origStderr
		log.SetOutput(origLog)
	}()
	fn()
	_ = w.Close()
	return string(<-read) + logged.String()
}

func TestCalendarFeedURLIsNeverEchoed(t *testing.T) {
	origTimeout := calendarFetchTimeout
	calendarFetchTimeout = 300 * time.Millisecond
	defer func() { calendarFetchTimeout = origTimeout }()

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	var thirdPartyMu sync.Mutex
	var thirdPartySeen []string
	thirdParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var dump bytes.Buffer
		_ = r.Header.Write(&dump)
		thirdPartyMu.Lock()
		thirdPartySeen = append(thirdPartySeen, r.URL.String()+"\n"+dump.String())
		thirdPartyMu.Unlock()
		http.Error(w, "gone", http.StatusGone)
	}))
	defer thirdParty.Close()

	feedPath := "/calendar/ical/" + feedMarker + "/private-" + feedMarker + "/basic.ics?token=" + feedMarker
	oversized := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nX-PAD:" + strings.Repeat("a", maxICSBodyBytes) + "\r\nEND:VCALENDAR\r\n"
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0] {
		case "status":
			http.Error(w, "forbidden", http.StatusForbidden)
		case "oversized":
			_, _ = io.WriteString(w, oversized)
		case "malformed":
			_, _ = io.WriteString(w, "this is not an iCalendar document")
		case "timeout":
			<-r.Context().Done()
		case "stalled-body":
			_, _ = io.WriteString(w, "BEGIN:VCALENDAR\r\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "redirect-loop":
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
		case "redirect-unreachable":
			http.Redirect(w, r, deadURL+"/next", http.StatusFound)
		case "redirect-third-party":
			http.Redirect(w, r, thirdParty.URL+"/next", http.StatusFound)
		}
	}))
	defer feed.Close()

	cases := []struct {
		name, url, status, detail string
	}{
		{"unusable URL", "ftp://example.invalid" + feedPath, StatusFailed, "not a usable absolute http(s) URL"},
		{"unreachable host", deadURL + feedPath, StatusFailed, "could not be fetched"},
		{"non-200 status", feed.URL + "/status" + feedPath, StatusFailed, "HTTP status 403"},
		{"oversized body", feed.URL + "/oversized" + feedPath, StatusIncomplete, "feed entry or expansion issue"},
		{"malformed ICS", feed.URL + "/malformed" + feedPath, StatusFailed, "did not parse"},
		{"timeout", feed.URL + "/timeout" + feedPath, StatusFailed, "could not be fetched"},
		{"timeout while reading the body", feed.URL + "/stalled-body" + feedPath, StatusFailed, "could not be read"},
		{"redirect loop", feed.URL + "/redirect-loop" + feedPath, StatusFailed, "could not be fetched"},
		{"redirect to an unreachable host", feed.URL + "/redirect-unreachable" + feedPath, StatusFailed, "could not be fetched"},
		{"redirect to a third party", feed.URL + "/redirect-third-party" + feedPath, StatusFailed, "HTTP status 410"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.url, feedMarker+"/") || !strings.Contains(tc.url, "token="+feedMarker) {
				t.Fatalf("the feed URL must carry the marker in its path and query: %s", tc.url)
			}
			dir := t.TempDir()
			cfg := &Config{
				Timezone:  "UTC",
				StatePath: filepath.Join(dir, "state.json"),
				Sources:   &SourcesConfig{Enabled: true, Calendar: &CalendarSourceConfig{Enabled: true}},
			}
			store, err := OpenStore(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			s := NewSession(cfg, map[string]string{"GCAL_ICS_URL": tc.url}, store)
			day := "2026-10-22"
			if _, err := s.openDay(Request{ID: "1", Op: "open_day", Day: day}); err != nil {
				t.Fatal(err)
			}

			var col CollectResult
			var colErr error
			stderr := captureStderr(t, func() {
				col, colErr = s.collectDay(Request{ID: "2", Op: "collect", Day: day})
			})
			if colErr != nil {
				t.Fatalf("collect: %v", colErr)
			}
			if len(col.Sources) != 1 || col.Sources[0].Status != tc.status || !strings.Contains(col.Sources[0].Detail, tc.detail) {
				t.Fatalf("calendar summary %+v; want status %s with detail containing %q", col.Sources, tc.status, tc.detail)
			}

			surfaces := map[string]string{"stderr": stderr}
			summary, err := json.Marshal(col)
			if err != nil {
				t.Fatal(err)
			}
			surfaces["collect result"] = string(summary)
			evidence, err := json.Marshal(store.Evidence(day))
			if err != nil {
				t.Fatal(err)
			}
			surfaces["stored evidence"] = string(evidence)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				surfaces["state file "+filepath.Base(path)] = string(data)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			thirdPartyMu.Lock()
			surfaces["redirect target requests"] = strings.Join(thirdPartySeen, "\n")
			thirdPartySeen = nil
			thirdPartyMu.Unlock()

			for name, text := range surfaces {
				if strings.Contains(text, feedMarker) {
					t.Errorf("the feed URL leaked into the %s: %s", name, text)
				}
			}
		})
	}
}

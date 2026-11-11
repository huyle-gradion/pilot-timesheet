package timesheet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(path string) *Config {
	return &Config{
		Timezone:  "Europe/Amsterdam",
		StatePath: path,
	}
}

func call(t *testing.T, s *Session, req Request) Response {
	t.Helper()
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, _ := s.HandleLine(line)
	return resp
}

func revision(n int) *int { return &n }

func requireOK(t *testing.T, resp Response) {
	t.Helper()
	if !resp.OK {
		if resp.Error != nil {
			t.Fatalf("unexpected error %s: %s", resp.Error.Code, resp.Error.Message)
		}
		t.Fatal("unexpected error response")
	}
}

func requireCode(t *testing.T, resp Response, code string) {
	t.Helper()
	if resp.OK || resp.Error == nil {
		t.Fatalf("expected error %s, got success", code)
	}
	if resp.Error.Code != code {
		t.Fatalf("expected error %s, got %s: %s", code, resp.Error.Code, resp.Error.Message)
	}
}

func TestTimezoneEndpointsAndAllocations(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		iv    Interval
		want  int
		code  string
		alloc []Allocation
	}{
		{
			name: "spring forward duration",
			iv:   Interval{Date: "2026-03-29", Start: "01:30", End: "03:30"},
			want: 60,
		},
		{
			name: "nonexistent spring time",
			iv:   Interval{Date: "2026-03-29", Start: "02:15", End: "02:45"},
			code: CodeNonexistentTime,
		},
		{
			name: "fall back duration includes repeated hour",
			iv:   Interval{Date: "2026-10-25", Start: "01:30", End: "03:30"},
			want: 180,
		},
		{
			name: "ambiguous fall time",
			iv:   Interval{Date: "2026-10-25", Start: "02:15", End: "02:45"},
			code: CodeAmbiguousTime,
		},
		{
			name: "cross midnight spring transition",
			iv:   Interval{Date: "2026-03-28", Start: "23:00", End: "03:00"},
			want: 180,
			alloc: []Allocation{
				{Date: "2026-03-28", Start: "23:00", End: "24:00", Minutes: 60},
				{Date: "2026-03-29", Start: "00:00", End: "03:00", Minutes: 120},
			},
		},
		{
			name: "cross midnight fall transition",
			iv:   Interval{Date: "2026-10-24", Start: "23:00", End: "03:30"},
			want: 330,
			alloc: []Allocation{
				{Date: "2026-10-24", Start: "23:00", End: "24:00", Minutes: 60},
				{Date: "2026-10-25", Start: "00:00", End: "03:30", Minutes: 270},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.iv.Duration(loc)
			if tc.code != "" {
				if err == nil || errorCode(err) != tc.code {
					t.Fatalf("expected %s, got %v", tc.code, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("duration: %v", err)
			}
			if got != tc.want {
				t.Fatalf("duration: got %d, want %d", got, tc.want)
			}
			if tc.alloc == nil {
				return
			}
			allocs, err := tc.iv.ResolvedAllocations(loc)
			if err != nil {
				t.Fatalf("allocations: %v", err)
			}
			if len(allocs) != len(tc.alloc) {
				t.Fatalf("allocations: got %+v", allocs)
			}
			for i := range tc.alloc {
				if allocs[i] != tc.alloc[i] {
					t.Fatalf("allocation %d: got %+v, want %+v", i, allocs[i], tc.alloc[i])
				}
			}
		})
	}
}

func TestSkippedMidnightDayBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		timezone string
		iv       Interval
		want     int
		alloc    []Allocation
		code     string
	}{
		{
			name:     "santiago before skipped midnight",
			timezone: "America/Santiago",
			iv:       Interval{Date: "2026-09-05", Start: "09:00", End: "10:00"},
			want:     60,
			alloc:    []Allocation{{Date: "2026-09-05", Start: "09:00", End: "10:00", Minutes: 60}},
		},
		{
			name:     "santiago after skipped midnight",
			timezone: "America/Santiago",
			iv:       Interval{Date: "2026-09-06", Start: "09:00", End: "10:00"},
			want:     60,
			alloc:    []Allocation{{Date: "2026-09-06", Start: "09:00", End: "10:00", Minutes: 60}},
		},
		{
			name:     "havana after skipped midnight",
			timezone: "America/Havana",
			iv:       Interval{Date: "2026-03-08", Start: "09:00", End: "10:00"},
			want:     60,
			alloc:    []Allocation{{Date: "2026-03-08", Start: "09:00", End: "10:00", Minutes: 60}},
		},
		{
			name:     "santiago spill across skipped midnight",
			timezone: "America/Santiago",
			iv:       Interval{Date: "2026-09-05", Start: "23:00", End: "01:30"},
			want:     90,
			alloc: []Allocation{
				{Date: "2026-09-05", Start: "23:00", End: "24:00", Minutes: 60},
				{Date: "2026-09-06", Start: "01:00", End: "01:30", Minutes: 30},
			},
		},
		{
			name:     "santiago endpoint on skipped wall time",
			timezone: "America/Santiago",
			iv:       Interval{Date: "2026-09-05", Start: "09:00", End: "00:15"},
			code:     CodeNonexistentTime,
		},
		{
			name:     "santiago start in missing midnight hour",
			timezone: "America/Santiago",
			iv:       Interval{Date: "2026-09-06", Start: "00:45", End: "09:00"},
			code:     CodeNonexistentTime,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.timezone)
			if err != nil {
				t.Fatal(err)
			}
			if tc.code != "" {
				_, err := tc.iv.Duration(loc)
				if err == nil || errorCode(err) != tc.code {
					t.Fatalf("expected %s, got %v", tc.code, err)
				}
				return
			}
			got, err := tc.iv.Duration(loc)
			if err != nil {
				t.Fatalf("duration: %v", err)
			}
			if got != tc.want {
				t.Fatalf("duration: got %d, want %d", got, tc.want)
			}
			allocs, err := tc.iv.ResolvedAllocations(loc)
			if err != nil {
				t.Fatalf("allocations: %v", err)
			}
			if len(allocs) != len(tc.alloc) {
				t.Fatalf("allocations: got %+v", allocs)
			}
			for i := range tc.alloc {
				if allocs[i] != tc.alloc[i] {
					t.Fatalf("allocation %d: got %+v, want %+v", i, allocs[i], tc.alloc[i])
				}
			}
			var total int
			for _, a := range allocs {
				total += a.Minutes
			}
			if total != tc.want {
				t.Fatalf("allocation total %d, elapsed %d", total, tc.want)
			}
		})
	}
}

func TestWhollyAbsentCivilDateIsNotFabricated(t *testing.T) {
	// Pacific/Apia skipped the whole civil date 2011-12-30 (24-hour jump).
	loc, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := localDayStart("2011-12-30", loc); err == nil || errorCode(err) != CodeInvalidInterval {
		t.Fatalf("expected invalid_interval for the absent civil date, got %v", err)
	}
	// A skipped midnight is not an absent date: September 6 exists and starts
	// at its first real instant, local 01:00 in Santiago.
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	start, err := localDayStart("2026-09-06", santiago)
	if err != nil {
		t.Fatalf("missing midnight treated as absent date: %v", err)
	}
	if got := start.In(santiago).Format("2006-01-02 15:04"); got != "2026-09-06 01:00" {
		t.Fatalf("first real instant of 2026-09-06 = %s, want 2026-09-06 01:00", got)
	}
}

func TestFinalIntervalRemovalAndTimezoneReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(testConfig(path), map[string]string{}, store)

	requireOK(t, call(t, session, Request{ID: "1", Op: "open_day", Day: "2026-10-30"}))
	requireOK(t, call(t, session, Request{
		ID: "2", Op: "add_interval", Day: "2026-10-30", Revision: revision(0),
		Text: "2026-10-30 09:00-10:00 work",
	}))
	confirm := call(t, session, Request{ID: "3", Op: "confirm", Day: "2026-10-30", Revision: revision(0)})
	requireOK(t, confirm)
	if receipt := confirm.Result.(ConfirmReceipt); receipt.TotalMinutes != 60 {
		t.Fatalf("confirm total = %d", receipt.TotalMinutes)
	}
	requireOK(t, call(t, session, Request{ID: "4", Op: "save", Day: "2026-10-30", Revision: revision(1)}))

	removed := call(t, session, Request{
		ID: "7", Op: "remove_interval", Day: "2026-10-30", Revision: revision(1),
		IntervalID: "iv-20261030-1",
	})
	requireOK(t, removed)
	empty := call(t, session, Request{ID: "8", Op: "confirm", Day: "2026-10-30", Revision: revision(1)})
	requireOK(t, empty)
	if got := empty.Result.(ConfirmReceipt); got.TotalMinutes != 0 || got.Intervals != 0 {
		t.Fatalf("empty confirm: %+v", got)
	}
	saved := call(t, session, Request{ID: "9", Op: "save", Day: "2026-10-30", Revision: revision(2)})
	requireOK(t, saved)
	if got := saved.Result.(SaveReceipt); got.TotalMinutes != 0 || got.Intervals != 0 {
		t.Fatalf("empty save: %+v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := testConfig(path)
	cfg.Timezone = "UTC"
	session = NewSession(cfg, map[string]string{}, store)
	opened := call(t, session, Request{ID: "10", Op: "open_day", Day: "2026-10-30"})
	requireOK(t, opened)
	summary := opened.Result.(DaySummary)
	if summary.Timezone != "Europe/Amsterdam" {
		t.Fatalf("timezone changed on reload: %s", summary.Timezone)
	}
	if summary.Revision != 2 || summary.SavedRevision != 2 || summary.Confirmed.TotalMinutes != 0 {
		t.Fatalf("restart summary: %+v", summary)
	}
	review := call(t, session, Request{ID: "11", Op: "review", Day: "2026-10-30"})
	requireOK(t, review)
	if got := len(review.Result.(Review).History); got != 2 {
		t.Fatalf("history entries = %d, want 2", got)
	}
}

func TestSaveUsesDurableCandidateAndKeepsReceiptsRestartSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(testConfig(path), map[string]string{}, store)
	defer store.Close()

	requireOK(t, call(t, session, Request{ID: "1", Op: "open_day", Day: "2026-11-02"}))
	requireOK(t, call(t, session, Request{
		ID: "2", Op: "add_interval", Day: "2026-11-02", Revision: revision(0),
		Text: "2026-11-02 23:00-02:00 work",
	}))
	requireOK(t, call(t, session, Request{ID: "3", Op: "confirm", Day: "2026-11-02", Revision: revision(0)}))
	requireOK(t, call(t, session, Request{ID: "4", Op: "save", Day: "2026-11-02", Revision: revision(1)}))

	requireOK(t, call(t, session, Request{
		ID: "5", Op: "edit_interval", Day: "2026-11-02", Revision: revision(1),
		IntervalID: "iv-20261102-1", Text: "2026-11-02 23:00-01:00 work",
	}))
	requireOK(t, call(t, session, Request{ID: "6", Op: "confirm", Day: "2026-11-02", Revision: revision(1)}))

	requireOK(t, call(t, session, Request{ID: "7", Op: "open_day", Day: "2026-11-03"}))
	requireOK(t, call(t, session, Request{
		ID: "8", Op: "add_interval", Day: "2026-11-03", Revision: revision(0),
		Text: "2026-11-03 01:00-02:00 work",
	}))
	requireOK(t, call(t, session, Request{ID: "9", Op: "confirm", Day: "2026-11-03", Revision: revision(0)}))

	blocked := call(t, session, Request{ID: "10", Op: "save", Day: "2026-11-03", Revision: revision(1)})
	requireCode(t, blocked, CodeOverlap)
	if _, exists := store.State().Days["2026-11-03"]; exists {
		t.Fatal("blocked save leaked a day into the durable snapshot")
	}

	requireOK(t, call(t, session, Request{ID: "11", Op: "save", Day: "2026-11-02", Revision: revision(2)}))
	saved := call(t, session, Request{ID: "12", Op: "save", Day: "2026-11-03", Revision: revision(1)})
	requireOK(t, saved)
	if got := saved.Result.(SaveReceipt).TotalMinutes; got != 120 {
		t.Fatalf("day-3 receipt total = %d, want 120", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	session = NewSession(testConfig(path), map[string]string{}, store)
	opened := call(t, session, Request{ID: "13", Op: "open_day", Day: "2026-11-03"})
	requireOK(t, opened)
	if got := opened.Result.(DaySummary).Confirmed.TotalMinutes; got != 120 {
		t.Fatalf("restart day-3 total = %d, want 120", got)
	}

	requireOK(t, call(t, session, Request{ID: "20", Op: "open_day", Day: "2026-11-04"}))
	requireOK(t, call(t, session, Request{
		ID: "21", Op: "add_interval", Day: "2026-11-04", Revision: revision(0),
		Text: "2026-11-04 23:00-01:00 work",
	}))
	requireOK(t, call(t, session, Request{ID: "22", Op: "confirm", Day: "2026-11-04", Revision: revision(0)}))
	requireOK(t, call(t, session, Request{ID: "23", Op: "open_day", Day: "2026-11-05"}))
	requireOK(t, call(t, session, Request{
		ID: "24", Op: "add_interval", Day: "2026-11-05", Revision: revision(0),
		Text: "2026-11-05 01:00-02:00 work",
	}))
	requireOK(t, call(t, session, Request{ID: "25", Op: "confirm", Day: "2026-11-05", Revision: revision(0)}))
	dependent := call(t, session, Request{ID: "26", Op: "save", Day: "2026-11-05", Revision: revision(1)})
	requireOK(t, dependent)
	if got := dependent.Result.(SaveReceipt).TotalMinutes; got != 60 {
		t.Fatalf("unsaved origin influenced receipt: %d", got)
	}
	requireOK(t, call(t, session, Request{ID: "27", Op: "save", Day: "2026-11-04", Revision: revision(1)}))
	after := call(t, session, Request{ID: "28", Op: "review", Day: "2026-11-05"})
	requireOK(t, after)
	if got := after.Result.(Review).Staged.TotalMinutes; got != 120 {
		t.Fatalf("derived spill after origin save = %d, want 120", got)
	}
}

func TestHistoryTotalMatchesDurableSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(testConfig(path), map[string]string{}, store)

	requireOK(t, call(t, session, Request{ID: "1", Op: "open_day", Day: "2026-11-10"}))
	requireOK(t, call(t, session, Request{
		ID: "2", Op: "add_interval", Day: "2026-11-10", Revision: revision(0),
		Text: "2026-11-10 23:00-01:00 durable origin",
	}))
	requireOK(t, call(t, session, Request{ID: "3", Op: "confirm", Day: "2026-11-10", Revision: revision(0)}))
	requireOK(t, call(t, session, Request{ID: "4", Op: "save", Day: "2026-11-10", Revision: revision(1)}))
	requireOK(t, call(t, session, Request{
		ID: "5", Op: "edit_interval", Day: "2026-11-10", Revision: revision(1),
		IntervalID: "iv-20261110-1", Text: "2026-11-10 23:00-02:00 unsaved extension",
	}))
	requireOK(t, call(t, session, Request{ID: "6", Op: "confirm", Day: "2026-11-10", Revision: revision(1)}))

	requireOK(t, call(t, session, Request{ID: "7", Op: "open_day", Day: "2026-11-11"}))
	requireOK(t, call(t, session, Request{
		ID: "8", Op: "add_interval", Day: "2026-11-11", Revision: revision(0),
		Text: "2026-11-11 02:00-03:00 dependent",
	}))
	requireOK(t, call(t, session, Request{ID: "9", Op: "confirm", Day: "2026-11-11", Revision: revision(0)}))
	saved := call(t, session, Request{ID: "10", Op: "save", Day: "2026-11-11", Revision: revision(1)})
	requireOK(t, saved)
	if got := saved.Result.(SaveReceipt).TotalMinutes; got != 120 {
		t.Fatalf("durable total = %d, want 120", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session = NewSession(testConfig(path), map[string]string{}, store)
	requireOK(t, call(t, session, Request{ID: "10", Op: "open_day", Day: "2026-11-11"}))
	review := call(t, session, Request{ID: "11", Op: "review", Day: "2026-11-11"})
	requireOK(t, review)
	got := review.Result.(Review)
	if len(got.History) != 1 || got.History[0].TotalMinutes != 120 {
		t.Fatalf("restart history = %+v, want one 120-minute entry", got.History)
	}
}

func TestPreRenameFailurePreservesOldStateAndAllowsRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := NewSession(testConfig(path), map[string]string{}, store)

	requireOK(t, call(t, session, Request{ID: "1", Op: "open_day", Day: "2026-12-01"}))
	requireOK(t, call(t, session, Request{
		ID: "2", Op: "add_interval", Day: "2026-12-01", Revision: revision(0),
		Text: "2026-12-01 09:00-10:00 work",
	}))
	requireOK(t, call(t, session, Request{ID: "3", Op: "confirm", Day: "2026-12-01", Revision: revision(0)}))
	requireOK(t, call(t, session, Request{ID: "4", Op: "save", Day: "2026-12-01", Revision: revision(1)}))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	requireOK(t, call(t, session, Request{
		ID: "5", Op: "edit_interval", Day: "2026-12-01", Revision: revision(1),
		IntervalID: "iv-20261201-1", Text: "2026-12-01 09:00-11:00 work",
	}))
	requireOK(t, call(t, session, Request{ID: "6", Op: "confirm", Day: "2026-12-01", Revision: revision(1)}))
	storeFaultHook = func(stage string) error {
		if stage == "before_rename" {
			return os.ErrPermission
		}
		return nil
	}
	defer func() { storeFaultHook = nil }()
	resp := call(t, session, Request{ID: "7", Op: "save", Day: "2026-12-01", Revision: revision(2)})
	requireCode(t, resp, CodeWriteFailed)
	t.Logf("pre-rename response code=%s", resp.Error.Code)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("pre-rename failure changed the durable file")
	}
	if got := store.State().Days["2026-12-01"].Revision; got != 1 {
		t.Fatalf("in-memory revision after rollback = %d, want 1", got)
	}
	t.Logf("on-disk bytes unchanged; in-memory revision rolled back to 1")

	storeFaultHook = nil
	retry := call(t, session, Request{ID: "8", Op: "save", Day: "2026-12-01", Revision: revision(2)})
	requireOK(t, retry)
	if got := retry.Result.(SaveReceipt).TotalMinutes; got != 120 {
		t.Fatalf("retry total = %d, want 120", got)
	}
	t.Logf("retry after clearing fault saved revision 2 with 120 minutes")
}

func TestPostRenameUncertaintyBlocksBlindRetryAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(testConfig(path), map[string]string{}, store)

	requireOK(t, call(t, session, Request{ID: "1", Op: "open_day", Day: "2026-12-02"}))
	requireOK(t, call(t, session, Request{
		ID: "2", Op: "add_interval", Day: "2026-12-02", Revision: revision(0),
		Text: "2026-12-02 09:00-10:00 work",
	}))
	requireOK(t, call(t, session, Request{ID: "3", Op: "confirm", Day: "2026-12-02", Revision: revision(0)}))
	requireOK(t, call(t, session, Request{ID: "4", Op: "save", Day: "2026-12-02", Revision: revision(1)}))

	requireOK(t, call(t, session, Request{
		ID: "5", Op: "edit_interval", Day: "2026-12-02", Revision: revision(1),
		IntervalID: "iv-20261202-1", Text: "2026-12-02 09:00-11:00 work",
	}))
	requireOK(t, call(t, session, Request{ID: "6", Op: "confirm", Day: "2026-12-02", Revision: revision(1)}))

	storeFaultHook = func(stage string) error {
		if stage == "directory_sync" {
			return os.ErrInvalid
		}
		return nil
	}
	defer func() { storeFaultHook = nil }()
	resp := call(t, session, Request{ID: "7", Op: "save", Day: "2026-12-02", Revision: revision(2)})
	requireCode(t, resp, CodeWriteUncertain)
	t.Logf("post-rename response code=%s", resp.Error.Code)

	status := call(t, session, Request{ID: "8", Op: "status"})
	requireOK(t, status)
	if !status.Result.(StatusResult).DurabilityUncertain {
		t.Fatal("status did not report uncertain durability")
	}
	if got := store.State().Days["2026-12-02"].Revision; got != 2 {
		t.Fatalf("in-memory activated revision = %d, want 2", got)
	}
	retry := call(t, session, Request{ID: "9", Op: "save", Day: "2026-12-02", Revision: revision(2)})
	requireCode(t, retry, CodeWriteUncertain)
	t.Logf("on-disk activated revision=2; status uncertain=true; blind retry code=%s", retry.Error.Code)
	storeFaultHook = nil

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session = NewSession(testConfig(path), map[string]string{}, store)
	opened := call(t, session, Request{ID: "10", Op: "open_day", Day: "2026-12-02"})
	requireOK(t, opened)
	if got := opened.Result.(DaySummary).Confirmed.TotalMinutes; got != 120 {
		t.Fatalf("recovered total = %d, want 120", got)
	}
	if got := store.State().Days["2026-12-02"].Revision; got != 2 {
		t.Fatalf("recovered revision = %d, want 2", got)
	}
	t.Logf("restart recovered revision=2 with 120 minutes")
}

func TestStateWithAmbiguousSavedTimeIsRejected(t *testing.T) {
	state := `{"schema":1,"days":{"2026-10-25":{"date":"2026-10-25","timezone":"Europe/Amsterdam","revision":1,"next_interval":2,"confirmed":[{"id":"iv-20261025-1","date":"2026-10-25","start":"02:15","end":"02:45","project":"acme","description":"ambiguous"}]}}}`
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if store != nil {
		_ = store.Close()
	}
	if err == nil || !strings.Contains(err.Error(), CodeStateInvalid) {
		t.Fatalf("expected state_invalid, got %v", err)
	}
}

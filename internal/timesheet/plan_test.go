package timesheet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
)

// fakeMCP is a loopback MCP provider that declares ONLY the two read tools and
// serves synthetic week/classification documents. It is a test fixture, not a
// product path.
type fakeMCP struct {
	mu          sync.Mutex
	day         string
	entries     []map[string]any
	gap         bool
	omitEntries bool
	nullEntries bool
	days        map[string][]map[string]any
	weekError   bool
	catOptions  []map[string]any
	catText     string
	catError    bool
	calls       []string

	// R4 verified-submission fixture state. Zero values preserve the read-only
	// two-tool fixture behavior that earlier tests rely on.
	writeTools bool
	failMode   string // "", "tool", "malformed", "reset", "ackonly-noentry"
	// wrongAckID makes edit acknowledgments name a DIFFERENT entry id than the
	// actually mutated one (R4F4 fixture); the read-back still shows the real
	// mutated entry under its true id.
	wrongAckID string
	injectWeek func() // runs (mutex held) before each week read; simulates external changes
	// injectCat runs (mutex held) when the classifications tool is served. It
	// lets a test arm a local durability fault AFTER the submission's fresh
	// preview has persisted, so the fault lands at the consumption barrier
	// rather than at preview persistence (R4F7 in-process boundary).
	injectCat   func()
	writeCalls  []string
	writeBodies []map[string]any
	nextCreate  int
}

// setWriteMultiDays enables the two write tools and serves a mutable multi-day
// week document.
func (f *fakeMCP) setWriteMultiDays(days map[string][]map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeTools = true
	f.days = days
	f.gap, f.omitEntries, f.nullEntries, f.weekError = false, false, false, false
}

func (f *fakeMCP) setWeek(entries []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = entries
	f.gap = false
	f.omitEntries = false
	f.days = nil
	f.weekError = false
}

// setMultiDays serves a week document with several explicit dates. An empty
// entry list is a valid empty day.
func (f *fakeMCP) setMultiDays(days map[string][]map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.days = days
	f.gap = false
	f.omitEntries = false
	f.weekError = false
}

// setMissingEntriesContainer serves the target day WITHOUT the required entries
// list, which is missing data, never an empty day.
func (f *fakeMCP) setMissingEntriesContainer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.omitEntries = true
	f.gap = false
	f.days = nil
	f.weekError = false
}

// nonNullEntries serves the required entries container as null.
func (f *fakeMCP) setNullEntries() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.days = nil
	f.nullEntries = true
	f.gap = false
	f.weekError = false
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *int64          `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
		}
	case "tools/list":
		names := []any{
			map[string]any{"name": toolGetMyWeek},
			map[string]any{"name": toolGetClassifications},
		}
		if f.writeTools {
			names = append(names,
				map[string]any{"name": toolLogTime},
				map[string]any{"name": toolEditTime})
		}
		result = map[string]any{"tools": names}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		f.calls = append(f.calls, p.Name)
		switch p.Name {
		case toolGetMyWeek:
			if f.injectWeek != nil {
				f.injectWeek()
			}
			if f.weekError {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "synthetic tool failure"}}}
				break
			}
			days := []any{}
			switch {
			case f.gap:
				// no days at all
			case f.nullEntries:
				days = append(days, map[string]any{"date": f.day, "entries": nil})
			case f.omitEntries:
				days = append(days, map[string]any{"date": f.day})
			case len(f.days) > 0:
				dates := make([]string, 0, len(f.days))
				for d := range f.days {
					dates = append(dates, d)
				}
				sort.Strings(dates)
				for _, d := range dates {
					entries := append([]map[string]any(nil), f.days[d]...)
					if entries == nil {
						entries = []map[string]any{}
					}
					days = append(days, map[string]any{"date": d, "entries": entries})
				}
			default:
				entries := f.entries
				if entries == nil {
					entries = []map[string]any{}
				}
				days = append(days, map[string]any{"date": f.day, "entries": entries})
			}
			result = map[string]any{
				"structuredContent": map[string]any{"days": days, "weekStart": f.day, "minDate": "2026-01-01", "maxDate": "2026-12-31"},
				"content":           []any{map[string]any{"type": "text", "text": "week"}},
			}
		case toolGetClassifications:
			if f.catError {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "synthetic catalog failure"}}}
				break
			}
			if f.injectCat != nil {
				f.injectCat()
			}
			result = map[string]any{
				"structuredContent": map[string]any{"resources": f.catOptions},
				"content":           []any{map[string]any{"type": "text", "text": f.catText}},
			}
		case toolLogTime, toolEditTime:
			if !f.writeTools {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unexpected tool"}}}
				break
			}
			f.writeCalls = append(f.writeCalls, p.Name)
			body := map[string]any{}
			for k, v := range p.Arguments {
				body[k] = v
			}
			f.writeBodies = append(f.writeBodies, body)
			switch f.failMode {
			case "tool":
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "synthetic write failure"}}}
			case "malformed":
				// No mutation: a well-formed HTTP 200 whose structured content
				// is a bare string, so no id can ever be decoded from it.
				result = map[string]any{
					"structuredContent": "not-an-object",
					"content":           []any{map[string]any{"type": "text", "text": "odd"}},
				}
			case "reset":
				// Applied-then-reset: the synthetic effect IS applied, then the
				// connection corrupts the response with trailing garbage, which
				// is a decode failure carrying no acknowledgment.
				_ = f.applyWrite(p.Name, body)
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
				_, _ = w.Write([]byte("SUFFIX-NOT-JSON"))
				return
			default:
				result = f.applyWrite(p.Name, body)
			}
		default:
			result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unexpected tool"}}}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
}

// applyWrite performs the synthetic mutation for one write call and returns a
// full structured acknowledgment ({"entry": {...}}).
func (f *fakeMCP) applyWrite(name string, body map[string]any) map[string]any {
	date, _ := body["date"].(string)
	start, _ := body["startTime"].(string)
	end, _ := body["endTime"].(string)
	desc, _ := body["description"].(string)
	class, _ := body["classificationId"].(string)
	if name == toolEditTime {
		id, _ := body["entryId"].(string)
		mutate := func(e map[string]any) map[string]any {
			res := mutateEntry(e, start, end, desc, class)
			if f.wrongAckID != "" {
				// R4F4 fixture: the provider acknowledges a different entry
				// identity than the one it actually mutated.
				sc := res["structuredContent"].(map[string]any)
				sc["entry"] = map[string]any{"id": f.wrongAckID, "startTime": e["startTime"], "endTime": e["endTime"],
					"classificationId": e["classificationId"], "description": e["description"], "status": "logged"}
			}
			return res
		}
		if f.days == nil {
			for _, e := range f.entries {
				if e["id"] == id {
					return mutate(e)
				}
			}
			return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "no such entry"}}}
		}
		for _, list := range f.days {
			for _, e := range list {
				if e["id"] == id {
					return mutate(e)
				}
			}
		}
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "no such entry"}}}
	}
	f.nextCreate++
	id := "r-new-" + strconv.Itoa(f.nextCreate)
	entry := map[string]any{
		"id": id, "startTime": start, "endTime": end,
		"classificationId": class, "description": desc, "status": "logged",
	}
	if f.days == nil {
		f.days = map[string][]map[string]any{}
	}
	f.days[date] = append(f.days[date], entry)
	return map[string]any{
		"structuredContent": map[string]any{"entry": map[string]any{"id": id, "startTime": start, "endTime": end, "classificationId": class, "description": desc, "status": "logged"}},
		"content":           []any{map[string]any{"type": "text", "text": "created"}},
	}
}

// mutateEntry applies approved-field edits to one synthetic entry and returns
// the structured full acknowledgment.
func mutateEntry(e map[string]any, start, end, desc, class string) map[string]any {
	if start != "" {
		e["startTime"] = start
	}
	if end != "" {
		e["endTime"] = end
	}
	if desc != "" {
		e["description"] = desc
	}
	if class != "" {
		e["classificationId"] = class
	}
	return map[string]any{
		"structuredContent": map[string]any{"entry": map[string]any{"id": e["id"], "startTime": e["startTime"], "endTime": e["endTime"], "classificationId": e["classificationId"], "description": e["description"], "status": "logged"}},
		"content":           []any{map[string]any{"type": "text", "text": "updated"}},
	}
}

func intervalFixture(t *testing.T, text string) (*Session, *fakeMCP, *Store, string) {
	t.Helper()
	fake := &fakeMCP{
		day: "2026-10-20",
		entries: []map[string]any{{
			"id": "r-1", "startTime": "09:00", "endTime": "10:00",
			"classificationId": "cat-1", "description": "Synthetic work", "status": "logged",
		}},
		catOptions: []map[string]any{{"id": "cat-1", "kind": "classification", "label": "Synthetic Category"}},
		catText:    "- Synthetic Category (SC) [Development] · cat-1",
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cfg := &Config{
		Timezone:  "UTC",
		StatePath: filepath.Join(t.TempDir(), "state.json"),
		Gradion:   &GradionConfig{Enabled: true, Endpoint: server.URL, TokenEnv: DefaultGradionTokenEnv},
	}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	env := map[string]string{DefaultGradionTokenEnv: "synthetic-token"}
	s := NewSession(cfg, env, store)
	if _, err := s.openDay(Request{ID: "1", Op: "open_day", Day: "2026-10-20"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.addInterval(Request{ID: "2", Op: "add_interval", Day: "2026-10-20", Revision: intPtr(0),
		Text: text}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "3", Op: "confirm", Day: "2026-10-20", Revision: intPtr(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "4", Op: "save", Day: "2026-10-20", Revision: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	return s, fake, store, "2026-10-20"
}

func planFixture(t *testing.T) (*Session, *fakeMCP, *Store, string) {
	t.Helper()
	return intervalFixture(t, "2026-10-20 09:00-10:00 Synthetic work")
}

// classifiedFixture stages, verifies the catalog, selects the catalog option and
// saves, so preview_plan can render a classed operation.
func classifiedFixture(t *testing.T, text string) (*Session, *fakeMCP, *Store, string) {
	t.Helper()
	s, fake, store, day := intervalFixture(t, text)
	cat, err := s.classifications(Request{ID: "5", Op: "classifications"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.selectClassification(Request{ID: "6", Op: "select_classification", Day: day,
		Revision: intPtr(1), IntervalID: "iv-20261020-1", CatalogID: cat.CatalogID,
		ClassificationID: "cat-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "7", Op: "confirm", Day: day, Revision: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "8", Op: "save", Day: day, Revision: intPtr(2)}); err != nil {
		t.Fatal(err)
	}
	return s, fake, store, day
}

// remoteEntry builds one synthetic observed remote entry.
func remoteEntry(id, start, end, description string) map[string]any {
	return map[string]any{
		"id": id, "startTime": start, "endTime": end,
		"classificationId": "cat-1", "description": description, "status": "logged",
	}
}

func intPtr(v int) *int { return &v }

func TestReconcileAdoptSelectAndExactPlan(t *testing.T) {
	s, fake, store, date := planFixture(t)

	// A fresh read alone never binds an unmapped entry.
	rec, err := s.reconcileDate(date)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Ready {
		t.Fatalf("reconcile must not be ready while identity is ambiguous: %+v", rec.Blockers)
	}
	if len(rec.Unbound) != 1 || rec.Unbound[0].RemoteID != "r-1" {
		t.Fatalf("unbound remote not surfaced: %+v", rec.Unbound)
	}
	if len(rec.Candidates) == 0 || rec.Candidates[0].Reason != "overlap" {
		t.Fatalf("overlap candidate not surfaced: %+v", rec.Candidates)
	}
	if len(store.State().Remote.Bindings) != 0 {
		t.Fatal("a read must never create a binding")
	}

	// A partial catalog cannot prove an option absent; a returned option is
	// selectable.
	cat, err := s.classifications(Request{ID: "5", Op: "classifications"})
	if err != nil {
		t.Fatal(err)
	}
	if cat.CatalogID == "" || len(cat.Options) != 1 {
		t.Fatalf("catalog not returned: %+v", cat)
	}
	if _, err := s.selectClassification(Request{ID: "6", Op: "select_classification", Day: date,
		Revision: intPtr(1), IntervalID: "iv-20261020-1", CatalogID: cat.CatalogID, ClassificationID: "cat-absent"}); err == nil {
		t.Fatal("an option the catalog never returned must be rejected")
	}
	if _, err := s.selectClassification(Request{ID: "7", Op: "select_classification", Day: date,
		Revision: intPtr(1), IntervalID: "iv-20261020-1", CatalogID: cat.CatalogID, ClassificationID: "cat-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "8", Op: "confirm", Day: date, Revision: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "9", Op: "save", Day: date, Revision: intPtr(2)}); err != nil {
		t.Fatal(err)
	}

	// Explicit adoption binds the exact observed entry.
	rec, err = s.reconcileDate(date)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.resolveRemoteRequest(Request{ID: "10", Op: "resolve_remote", Day: date, Revision: intPtr(2),
		ObservationID: rec.ObservationID, RemoteID: "r-1", LocalID: "iv-20261020-1", Decision: DecisionAdopt})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Bound || res.Complete != true {
		t.Fatalf("adoption receipt: %+v", res)
	}
	if len(store.State().Remote.Bindings) != 1 || store.State().Remote.Bindings[0].RemoteID != "r-1" {
		t.Fatal("adoption did not create the durable binding")
	}

	// Identical work and identical remote content yield no write operation, and
	// an identical fresh observation yields a stable plan identity.
	first, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != PlanReady || len(first.Operations) != 0 {
		t.Fatalf("unchanged mapped work must be ready with no operation: %+v", first)
	}
	second, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if second.PlanID != first.PlanID {
		t.Fatalf("identical observation changed the plan id: %s vs %s", first.PlanID, second.PlanID)
	}

	// A changed mapped remote cannot become ready silently.
	fake.setWeek([]map[string]any{{
		"id": "r-1", "startTime": "09:00", "endTime": "10:00",
		"classificationId": "cat-1", "description": "Changed remotely", "status": "logged",
	}})
	rec, err = s.reconcileDate(date)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Ready || len(rec.Changed) != 1 {
		t.Fatalf("changed mapping not surfaced/blocked: %+v", rec)
	}
	blocked, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State != PlanBlocked || !hasBlocker(blocked.Blockers, "remote_changed") {
		t.Fatalf("changed remote must block readiness: %+v", blocked)
	}

	// Explicit acceptance of the same mapped identity produces one exact edit.
	if _, err := s.resolveRemoteRequest(Request{ID: "11", Op: "resolve_remote", Day: date, Revision: intPtr(2),
		ObservationID: rec.ObservationID, RemoteID: "r-1", LocalID: "iv-20261020-1", Decision: DecisionAcceptObserved}); err != nil {
		t.Fatal(err)
	}
	edited, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if edited.State != PlanReady || len(edited.Operations) != 1 || edited.Operations[0].Kind != "edit" ||
		edited.Operations[0].RemoteID != "r-1" || edited.Operations[0].ExpectedRemote == nil {
		t.Fatalf("expected one exact edit: %+v", edited)
	}
	if edited.PlanID == first.PlanID {
		t.Fatal("changed remote content must change the plan identity")
	}
	if edited.Approved {
		t.Fatal("a plan preview is never approved on its own: approval is ledger state, and no approval exists here")
	}
	if !edited.ApprovalAvailable {
		t.Fatal("a ready plan reports approval_available as a derived ledger fact") // R4
	}

	// A stale observation id is rejected without mutation.
	if _, err := s.resolveRemoteRequest(Request{ID: "12", Op: "resolve_remote", Day: date, Revision: intPtr(2),
		ObservationID: "stale", RemoteID: "r-1", LocalID: "iv-20261020-1", Decision: DecisionAcceptObserved}); err == nil {
		t.Fatal("stale observation must be rejected")
	}
}

func TestIncompleteAndFailedReadsBlockAndReplaceObservation(t *testing.T) {
	s, fake, store, date := planFixture(t)
	if _, err := s.reconcileDate(date); err != nil {
		t.Fatal(err)
	}
	before := store.State().Remote.Observations[date]
	if before == nil || !before.Complete {
		t.Fatal("expected a complete first observation")
	}

	fake.mu.Lock()
	fake.gap = true
	fake.mu.Unlock()
	rec, err := s.reconcileDate(date)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Complete || rec.Ready || !hasBlocker(rec.Blockers, "remote_incomplete") {
		t.Fatalf("a missing range must be incomplete and block: %+v", rec)
	}
	after := store.State().Remote.Observations[date]
	if after.ObservationID == before.ObservationID || after.Complete {
		t.Fatal("the old complete observation is still current proof of absence")
	}
	plan, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if plan.State != PlanBlocked || len(plan.Operations) != 0 || !hasBlocker(plan.Blockers, "remote_incomplete") {
		t.Fatalf("incomplete coverage must block readiness: %+v", plan)
	}

	fake.mu.Lock()
	fake.gap = false
	fake.weekError = true
	fake.mu.Unlock()
	rec, err = s.reconcileDate(date)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Failed || rec.Ready || !hasBlocker(rec.Blockers, "remote_unreadable") {
		t.Fatalf("a failed read must be explicit: %+v", rec)
	}
	if !store.State().Remote.Observations[date].Failed {
		t.Fatal("failed observation was not persisted")
	}
}

func hasBlocker(blockers []Blocker, code string) bool {
	for _, b := range blockers {
		if b.Code == code {
			return true
		}
	}
	return false
}

// F1: a 23:00-01:00 interval renders as one 60-minute allocation per date. The
// later date's preview observes the later date's remote entries.
func TestCrossMidnightAllocationsSpanAndMinutes(t *testing.T) {
	s, fake, store, day := classifiedFixture(t, "2026-10-20 23:00-01:00 Overnight work")
	if day != "2026-10-20" {
		t.Fatal(day)
	}
	fake.setMultiDays(map[string][]map[string]any{
		"2026-10-20": {},
		"2026-10-21": {},
	})

	origin, err := s.previewPlan(day)
	if err != nil {
		t.Fatal(err)
	}
	if origin.State != PlanReady || len(origin.Operations) != 1 {
		t.Fatalf("origin allocation must be one ready create: %+v", origin)
	}
	op := origin.Operations[0]
	if op.LocalID != "iv-20261020-1" || op.Date != "2026-10-20" || op.StartTime != "23:00" ||
		op.EndTime != "00:00" || op.Minutes != 60 {
		t.Fatalf("origin allocation span/minutes wrong: %+v", op)
	}

	spill, err := s.previewPlan("2026-10-21")
	if err != nil {
		t.Fatal(err)
	}
	if spill.State != PlanReady || len(spill.Operations) != 1 {
		t.Fatalf("spill allocation must be one ready create: %+v", spill)
	}
	op = spill.Operations[0]
	if op.LocalID != "iv-20261020-1" || op.Date != "2026-10-21" || op.StartTime != "00:00" ||
		op.EndTime != "01:00" || op.Minutes != 60 {
		t.Fatalf("spill allocation span/minutes wrong: %+v", op)
	}
	if spill.SourceRevisions["2026-10-20"] == 0 {
		t.Fatalf("spill plan must bind all contributing source revisions: %+v", spill.SourceRevisions)
	}
	if len(store.State().Remote.Bindings) != 0 {
		t.Fatal("a preview never binds")
	}
}

// F1: the two allocations of ONE interval are independent identities. Each can
// be bound to a different remote ID without stealing the other mapping, and both
// bindings survive restart.
func TestCrossMidnightBindsBothAllocationsIndependently(t *testing.T) {
	s, fake, store, day := classifiedFixture(t, "2026-10-20 23:00-01:00 Overnight work")
	early := remoteEntry("r-night1", "23:00", "00:00", "Overnight work")
	late := remoteEntry("r-late", "00:00", "01:00", "Overnight work")
	fake.setMultiDays(map[string][]map[string]any{
		"2026-10-20": {early},
		"2026-10-21": {late},
	})

	rec, err := s.reconcileDate("2026-10-21")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Ready {
		t.Fatalf("the later-day entry must block the spill create: %+v", rec.Blockers)
	}
	if _, err := s.resolveRemoteRequest(Request{ID: "1", Op: "resolve_remote", Day: "2026-10-21",
		Revision:      intPtr(0),
		ObservationID: rec.ObservationID, RemoteID: "r-late", LocalID: "iv-20261020-1",
		Decision: DecisionAdopt}); err != nil {
		t.Fatalf("later-day adoption against its actual allocation must be possible: %v", err)
	}

	rec2, err := s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Ready {
		t.Fatalf("the origin-day entry must block the origin create: %+v", rec2.Blockers)
	}
	if _, err := s.resolveRemoteRequest(Request{ID: "2", Op: "resolve_remote", Day: day,
		Revision:      intPtr(2),
		ObservationID: rec2.ObservationID, RemoteID: "r-night1", LocalID: "iv-20261020-1",
		Decision: DecisionAdopt}); err != nil {
		t.Fatalf("origin-day adoption must stay available for its own allocation: %v", err)
	}

	bindings := store.State().Remote.Bindings
	if len(bindings) != 2 {
		t.Fatalf("both allocations must be bound: %+v", bindings)
	}
	seen := map[string]string{}
	for _, b := range bindings {
		if b.LocalID != "iv-20261020-1" {
			t.Fatalf("unexpected binding: %+v", b)
		}
		seen[b.Date] = b.RemoteID
	}
	if seen["2026-10-20"] != "r-night1" || seen["2026-10-21"] != "r-late" {
		t.Fatalf("bindings must be per allocation: %+v", seen)
	}

	// Identical fresh observations leave each date's work unchanged: ready, no
	// write, no re-binding.
	for _, d := range []string{day, "2026-10-21"} {
		plan, err := s.previewPlan(d)
		if err != nil {
			t.Fatal(err)
		}
		if plan.State != PlanReady || len(plan.Operations) != 0 {
			t.Fatalf("%s must be ready with no operation after adoption: %+v", d, plan)
		}
	}

	// Both bindings survive a restart and the store accepts the per-allocation
	// state.
	path := s.cfg.StatePath
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("the per-allocation binding state must load: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if len(reopened.State().Remote.Bindings) != 2 {
		t.Fatalf("restart lost a binding: %+v", reopened.State().Remote.Bindings)
	}
}

// F7: after the local identity is removed, its observed remote entry is a
// bound-but-orphaned slot. It stays surfaced as an orphan and still blocks an
// overlapping create across restart.
func TestRemovedIdentityOccupiedSlotBlocksCreate(t *testing.T) {
	s, _, store, day := classifiedFixture(t, "2026-10-20 09:00-10:00 Synthetic work")
	rec, err := s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveRemoteRequest(Request{ID: "1", Op: "resolve_remote", Day: day,
		Revision: intPtr(2), ObservationID: rec.ObservationID, RemoteID: "r-1",
		LocalID: "iv-20261020-1", Decision: DecisionAdopt}); err != nil {
		t.Fatal(err)
	}

	// Remove the local identity; the mapping must survive it (never erased) and
	// the observed entry stays an occupied slot.
	if _, err := s.openDay(Request{ID: "2", Op: "open_day", Day: day}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.removeInterval(Request{ID: "3", Op: "remove_interval", Day: day,
		Revision: intPtr(2), IntervalID: "iv-20261020-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "4", Op: "confirm", Day: day, Revision: intPtr(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "5", Op: "save", Day: day, Revision: intPtr(3)}); err != nil {
		t.Fatal(err)
	}
	if len(store.State().Remote.Bindings) != 1 {
		t.Fatal("removal must never erase or reassign a mapping")
	}
	rec, err = s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Orphaned) != 1 || rec.Orphaned[0].RemoteID != "r-1" {
		t.Fatalf("orphaned mapping not surfaced: %+v", rec.Orphaned)
	}

	// A replacement local allocation overlapping the occupied slot must stay
	// blocked with an explicit orphan identity blocker.
	if _, err := s.addInterval(Request{ID: "6", Op: "add_interval", Day: day, Revision: intPtr(3),
		Text: "2026-10-20 09:30-10:30 Replacement work"}); err != nil {
		t.Fatal(err)
	}
	cat, err := s.classifications(Request{ID: "7", Op: "classifications"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.selectClassification(Request{ID: "8", Op: "select_classification", Day: day,
		Revision: intPtr(3), IntervalID: "iv-20261020-2", CatalogID: cat.CatalogID,
		ClassificationID: "cat-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "9", Op: "confirm", Day: day, Revision: intPtr(3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "10", Op: "save", Day: day, Revision: intPtr(4)}); err != nil {
		t.Fatal(err)
	}
	rec, err = s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Ready || !hasBlocker(rec.Blockers, "orphaned_mapping") {
		t.Fatalf("an occupied slot must block a create: %+v", rec)
	}
	plan, err := s.previewPlan(day)
	if err != nil {
		t.Fatal(err)
	}
	if plan.State != PlanBlocked || len(plan.Operations) != 0 || !hasBlocker(plan.Blockers, "orphaned_mapping") {
		t.Fatalf("the orphan conflict must survive into the plan: %+v", plan)
	}
}

// F4: an exact current-observation foreign decision stops an equal-text hint
// from blocking, without granting permission to overlap occupied time, and it
// goes stale when the observation changes.
func TestForeignDecisionResolvesEqualTextNotOverlap(t *testing.T) {
	s, fake, _, day := classifiedFixture(t, "2026-10-20 09:00-10:00 Synthetic work")
	// r-late is a DIFFERENT remote entry that repeats the local text but does
	// not touch the local time.
	fake.setWeek([]map[string]any{remoteEntry("r-late", "11:00", "12:00", "Synthetic work")})
	rec, err := s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Ready || len(rec.Candidates) != 1 || rec.Candidates[0].Reason != "equal_text" {
		t.Fatalf("equal text must surface as a candidate: %+v", rec)
	}
	if _, err := s.resolveRemoteRequest(Request{ID: "1", Op: "resolve_remote", Day: day,
		Revision: intPtr(2), ObservationID: rec.ObservationID, RemoteID: "r-late",
		Decision: DecisionForeign}); err != nil {
		t.Fatal(err)
	}

	rec, err = s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Ready || len(rec.Candidates) != 0 {
		t.Fatalf("the foreign decision must stop blocking equal text: %+v", rec)
	}
	plan, err := s.previewPlan(day)
	if err != nil {
		t.Fatal(err)
	}
	if plan.State != PlanReady || len(plan.Operations) != 1 {
		t.Fatalf("a non-overlapping foreign-marked remote must not block the create: %+v", plan)
	}

	// An identical fresh observation keeps the decision; a CHANGED relevant
	// observation makes it stale and the hint returns.
	rec2, err := s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if !rec2.Ready || rec2.ObservationID != rec.ObservationID || len(rec2.Candidates) != 0 {
		t.Fatalf("a fresh identical observation must keep the decision: %+v", rec2)
	}
	fake.setWeek([]map[string]any{remoteEntry("r-late", "11:15", "12:15", "Synthetic work")})
	rec3, err := s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if rec3.Ready || len(rec3.Candidates) != 1 || rec3.Candidates[0].Reason != "equal_text" {
		t.Fatalf("a changed observation must make the foreign decision stale: %+v", rec3)
	}

	// Foreign never authorizes a physical overlap.
	fake.setWeek([]map[string]any{remoteEntry("r-late", "09:30", "10:30", "Synthetic work")})
	rec4, err := s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if rec4.Ready || len(rec4.Candidates) != 1 || rec4.Candidates[0].Reason != "overlap" {
		t.Fatalf("the foreign decision must not permit overlap: %+v", rec4)
	}
}

// F6: a blocked preview and the ready preview that follows an explicit decision
// are DIFFERENT identities, even with identical work and remote bytes. The ready
// record is durably persisted and the blocked record stays as history.
func TestPlanIdentitySeparatesReadyFromBlocked(t *testing.T) {
	s, fake, store, day := classifiedFixture(t, "2026-10-20 09:00-10:00 Synthetic work")
	fake.setWeek([]map[string]any{remoteEntry("r-hint", "11:00", "12:00", "Synthetic work")})

	blocked, err := s.previewPlan(day)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State != PlanBlocked || !hasBlocker(blocked.Blockers, "unresolved_identity") {
		t.Fatalf("equal text must keep the plan blocked: %+v", blocked)
	}
	savedBlocked := store.State().Plans[blocked.PlanID]
	if savedBlocked == nil || savedBlocked.State != PlanBlocked {
		t.Fatal("the blocked identity was not persisted")
	}

	rec, err := s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveRemoteRequest(Request{ID: "1", Op: "resolve_remote", Day: day,
		Revision: intPtr(2), ObservationID: rec.ObservationID, RemoteID: "r-hint",
		Decision: DecisionForeign}); err != nil {
		t.Fatal(err)
	}
	ready, err := s.previewPlan(day)
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != PlanReady || len(ready.Operations) != 1 {
		t.Fatalf("the plan must become ready with one create: %+v", ready)
	}
	if ready.PlanID == blocked.PlanID {
		t.Fatal("readiness preconditions must be part of the plan identity")
	}
	if !ready.Persisted {
		t.Fatal("the new ready identity must be durably persisted")
	}
	rec2, err := s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.previewPlan(day)
	if err != nil {
		t.Fatal(err)
	}
	if again.PlanID != ready.PlanID || again.Persisted {
		t.Fatalf("an identical fresh observation must be a stable no-change identity: %+v", again)
	}
	_ = rec2
	if stored := store.State().Plans[ready.PlanID]; stored == nil || stored.State != PlanReady ||
		len(stored.Operations) != 1 || stored.Operations[0].RemoteID != "" {
		t.Fatalf("the durable ready record does not match the preview: %+v", stored)
	}
	if len(store.State().Plans) != 2 {
		t.Fatalf("the blocked record must stay as immutable history: %+v", store.State().Plans)
	}
}

// F5: a target-day response WITHOUT the required entries container is missing
// data, never an empty day. It replaces any previously complete observation and
// cannot yield a ready create.
func TestMissingEntriesContainerBlocks(t *testing.T) {
	s, fake, store, day := classifiedFixture(t, "2026-10-20 09:00-10:00 Synthetic work")
	rec, err := s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Complete {
		t.Fatal("expected a complete first observation")
	}

	fake.setMissingEntriesContainer()
	rec, err = s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Failed || rec.Complete || !hasBlocker(rec.Blockers, "remote_unreadable") {
		t.Fatalf("a day object without entries must be unreadable, never empty: %+v", rec)
	}
	obs := store.State().Remote.Observations[day]
	if obs == nil || obs.Complete || !obs.Failed {
		t.Fatalf("the unreadable result must replace the complete one: %+v", obs)
	}
	plan, err := s.previewPlan(day)
	if err != nil {
		t.Fatal(err)
	}
	if plan.State != PlanBlocked || len(plan.Operations) != 0 || !hasBlocker(plan.Blockers, "remote_unreadable") {
		t.Fatalf("no ready create may be produced here: %+v", plan)
	}

	// entries:null and a malformed days shape also fail closed.
	fake.setNullEntries()
	rec, err = s.reconcileDate(day)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Complete || rec.Ready || !hasBlocker(rec.Blockers, "remote_unreadable") {
		t.Fatalf("entries:null is not an empty day: %+v", rec)
	}
}

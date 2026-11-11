package timesheet

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/mcp"
)

func TestRemoteErrorCarriesOnlyTheStableCode(t *testing.T) {
	for _, err := range []error{
		&mcp.Error{StableCode: "http_status", Message: "Bearer SECRET-TOKEN rejected"},
		errors.New("GET https://host/mcp?token=SECRET-TOKEN failed"),
	} {
		if got := remoteError(err).Error(); strings.Contains(got, "SECRET-TOKEN") {
			t.Fatalf("remoteError leaked transport detail: %q", got)
		}
	}
}

func TestParseWeekPayloadFailsClosedOnMissingContainers(t *testing.T) {
	for _, payload := range []string{
		"", "null", "[]", `{"weekStart":"2026-11-02"}`, `{"days":{}}`, `{"days":[1]}`,
		`{"days":[{"entries":[]}]}`, `{"days":[{"date":1,"entries":[]}]}`,
		`{"days":[{"date":"2026-11-02"}]}`, `{"days":[{"date":"2026-11-02","entries":null}]}`,
	} {
		if _, err := parseWeekPayload(mcp.CallResult{Structured: []byte(payload)}); err == nil {
			t.Fatalf("parseWeekPayload(%q) succeeded, want a read error", payload)
		}
	}
	if _, err := parseWeekPayload(mcp.CallResult{Structured: []byte(`{"days":[{"date":"2026-11-02","entries":[]}]}`)}); err != nil {
		t.Fatalf("explicit empty entries: %v", err)
	}
}

func TestDateObservationFromWeekCallReportsGaps(t *testing.T) {
	week := func(extra, entries string) mcp.CallResult {
		return mcp.CallResult{Structured: []byte(`{` + extra + `"days":[{"date":"2026-11-02","entries":[` + entries + `]}]}`)}
	}
	entry := `{"id":"r1","startTime":"09:00","endTime":"10:00"}`
	toolErr := week("", entry)
	toolErr.IsError = true
	if obs := dateObservationFromWeekCall("2026-11-02", toolErr, nil); !obs.Failed || obs.Complete {
		t.Fatalf("tool-level error observation = %+v, want failed", obs)
	}
	if obs := dateObservationFromWeekCall("2026-11-02", week("", entry+","+entry), nil); obs.Duplicates != 1 || obs.Complete {
		t.Fatalf("duplicate id observation = %+v, want one duplicate and incomplete", obs)
	}
	if obs := dateObservationFromWeekCall("2026-11-02", week(`"minDate":"2026-11-03",`, entry), nil); obs.Complete {
		t.Fatalf("before-minDate observation = %+v, want incomplete", obs)
	}
	if obs := dateObservationFromWeekCall("2026-11-02", week("", entry), nil); !obs.Complete || len(obs.Entries) != 1 {
		t.Fatalf("clean observation = %+v, want complete with one entry", obs)
	}
}

func TestParseCatalogMarksPartialCatalogsIncomplete(t *testing.T) {
	entry := "- Build (B1) [Development] · cls-1"
	var many []string
	for i := 0; i < maxCatalogOptions; i++ {
		many = append(many, fmt.Sprintf("- Opt %d (C%d) [Development] · cls-%d", i, i, i))
	}
	cases := map[string]mcp.CallResult{
		"truncated":       {Content: "120+ classifications, showing 1\n" + entry},
		"structured only": {Structured: []byte(`{"resources":[{"id":"cls-1","label":"Build"}]}`)},
		"no text entries": {Structured: []byte(`{"resources":[{"id":"cls-1","label":"Build"}]}`), Content: "1 classification found"},
		"at option cap":   {Content: strings.Join(many, "\n")},
	}
	for name, res := range cases {
		cat, err := parseCatalog(res, "", time.Now().UTC())
		if err != nil || cat.Complete {
			t.Fatalf("%s: catalog = %+v, %v; want an incomplete catalog", name, cat, err)
		}
	}
}

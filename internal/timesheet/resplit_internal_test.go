package timesheet

import (
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/mcp"
)

func TestSanitizeProviderDetail(t *testing.T) {
	raw := "Entry exceeds the 4 hour limit.\nSee https://workspace.gradion.com/api/mcp?token=ghp_secret123 for details.\r\n\x00"
	got := sanitizeProviderDetail(raw)
	if strings.Contains(got, "http") || strings.Contains(got, "ghp_secret123") {
		t.Fatalf("sanitizeProviderDetail leaked %q", got)
	}
	if strings.ContainsAny(got, "\n\r\x00") {
		t.Fatalf("sanitizeProviderDetail kept control characters: %q", got)
	}
	if !strings.Contains(got, "4 hour limit") {
		t.Fatalf("sanitizeProviderDetail = %q, want the provider reason preserved", got)
	}
	if got != truncateRunes(got, maxDetailRunes) {
		t.Fatalf("sanitizeProviderDetail = %q, want it rune-bounded", got)
	}
	long := sanitizeProviderDetail(strings.Repeat("x", maxDetailRunes*4))
	if n := len([]rune(long)); n > maxTitleRunes+1 {
		t.Fatalf("sanitizeProviderDetail bound = %d runes, want the documented 240 (+ellipsis)", n)
	}
}

func TestClassifyWriteAttemptCarriesDetail(t *testing.T) {
	res := mcp.CallResult{IsError: true, Content: "Entry longer than 4 hours is not allowed (max 240 minutes)"}
	attempt := classifyWriteAttempt(nil, res)
	if attempt.Status != IntentUncertain || attempt.Code != CodeWriteUncertain {
		t.Fatalf("attempt = %+v, want uncertain/%s", attempt, CodeWriteUncertain)
	}
	if !strings.Contains(attempt.Detail, "4 hours") || !strings.Contains(attempt.Detail, "240 minutes") {
		t.Fatalf("attempt.Detail = %q, want the provider's policy reason", attempt.Detail)
	}
	ok := mcp.CallResult{}
	if a := classifyWriteAttempt(nil, ok); a.Detail != "" {
		t.Fatalf("clean attempt carries detail %q", a.Detail)
	}
}

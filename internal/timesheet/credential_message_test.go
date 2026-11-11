package timesheet

import (
	"strings"
	"testing"
)

func TestMissingCredentialErrorNamesTheFix(t *testing.T) {
	err := missingCredentialError("SYNTHETIC_MISSING_TOKEN")
	msg := err.Error()
	for _, want := range []string{"SYNTHETIC_MISSING_TOKEN", "--env-file", "export it"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q must contain %q", msg, want)
		}
	}
}

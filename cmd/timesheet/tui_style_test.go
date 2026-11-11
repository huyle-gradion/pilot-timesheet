package main

import (
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// asciiRenderer pins the Ascii profile, where every pure-attribute style
// renders as identity; frames are hermetic against the host's TERM/CLICOLOR_FORCE.
func asciiRenderer() *lipgloss.Renderer {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.Ascii)
	return r
}

func testStyles() uiStyles { return newUIStyles(asciiRenderer()) }

func styledStyles() uiStyles {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.ANSI)
	return newUIStyles(r)
}

func stripANSI(frame string) string { return ansi.Strip(frame) }

func TestStylesEmitSGRPerRole(t *testing.T) {
	s := styledStyles()
	roles := map[string]func() string{
		"title":  func() string { return s.title.Render("x") },
		"label":  func() string { return s.label.Render("x") },
		"hint":   func() string { return s.hint.Render("x") },
		"errSty": func() string { return s.errSty.Render("x") },
		"gate":   func() string { return s.gate.Render("x") },
		"caret":  func() string { return s.caret.Render("x") },
	}
	for name, render := range roles {
		if !strings.Contains(render(), "\x1b[") {
			t.Fatalf("style role %q emitted no SGR under the ANSI profile", name)
		}
	}
}

func TestStylesArePresentAndTextNeutral(t *testing.T) {
	renders := []struct {
		name string
		fn   func(uiStyles) string
	}{
		{"day", func(s uiStyles) string {
			return renderDay(tuiModel{
				env: tuiEnv{styles: s}, err: errForTest("boom"),
				snap: loopSnapshot{day: "2026-10-20", action: nextAction{Day: "2026-10-20"}.withAction(
					"run", []string{"/bin/timesheet", "run"}, "why")},
			})
		}},
		{"bench", func(s uiStyles) string {
			return renderBench(tuiModel{env: tuiEnv{styles: s}, bench: benchState{
				screen: screenBench, kind: judgmentBlockDescriptions, digest: "d1",
				answerFile: "/tmp/x/descriptions.json", text: "{}", caret: 1,
			}})
		}},
		{"gate", func(s uiStyles) string {
			return renderGate(tuiModel{env: tuiEnv{styles: s}, bench: benchState{
				screen: screenGate, kind: gateApprove, answerFile: "type the exact plan_id above to approve",
				text: "plan-1", caret: 6,
				preview: &timesheet.PlanPreview{PlanID: "plan-1", State: timesheet.PlanReady},
			}})
		}},
	}
	for _, tc := range renders {
		t.Run(tc.name, func(t *testing.T) {
			ascii := tc.fn(testStyles())
			styled := tc.fn(styledStyles())
			if got := stripANSI(styled); got != ascii {
				t.Fatalf("styled frame is not text-neutral:\n--- ascii ---\n%q\n--- stripped styled ---\n%q", ascii, got)
			}
			if !strings.Contains(styled, "\x1b[") {
				t.Fatalf("no SGR reached the styled frame:\n%q", styled)
			}
		})
	}
}

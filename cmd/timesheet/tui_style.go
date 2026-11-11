package main

// The TUI's one style owner: the only production file that imports lipgloss.

import (
	"os"

	"github.com/charmbracelet/lipgloss"
)

type uiStyles struct {
	title  lipgloss.Style
	label  lipgloss.Style
	hint   lipgloss.Style
	errSty lipgloss.Style
	gate   lipgloss.Style
	caret  lipgloss.Style
}

func newUIStyles(r *lipgloss.Renderer) uiStyles {
	return uiStyles{
		title:  r.NewStyle().Bold(true),
		label:  r.NewStyle().Faint(true),
		hint:   r.NewStyle().Faint(true),
		errSty: r.NewStyle().Foreground(lipgloss.Color("1")),
		gate:   r.NewStyle().Bold(true),
		caret:  r.NewStyle().Reverse(true),
	}
}

// stdoutStyles binds the production styles to os.Stdout, the writer bubbletea's
// default output uses, so profile detection matches the frame's terminal.
func stdoutStyles() uiStyles {
	return newUIStyles(lipgloss.NewRenderer(os.Stdout))
}

package main

// The structural human gate (campaign issue #160, slice SUBMIT-4, design
// decision 2). approve, submit and resolve-write touch Gradion remotely on a
// human decision, so they are interactive-only: each refuses to run unless
// BOTH stdin and stdout are terminals, and each requires typing an exact id at
// an interactive prompt, so agent shells are not TTYs and physically cannot
// execute these commands. This is a deliberate-act barrier, not proof of human
// presence: a pty-allocating wrapper satisfies the terminal check and reaches
// the prompt — consistent with the internal approval ledger's own admission
// that "a scripted request is not proof a human is physically present". The
// check lives here, in the CLI wrapper only; the Session ops stay
// transport-agnostic.
//
// Prompt and display output goes to stderr, so stdout stays pure receipt JSON
// exactly as in every other subcommand. The gate still requires stdout itself
// to be a terminal, so redirecting stdout (or stdin) refuses the command
// before the store is opened and before any remote call.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// isTerminal reports whether f is attached to an interactive terminal. It
// performs a real tty ioctl (golang.org/x/term), not a character-device
// heuristic: pipes, redirected files and /dev/null all fail the gate.
func isTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

type interactive struct {
	in  *bufio.Reader
	out io.Writer
}

var interactiveFactory = newInteractive

// newInteractive builds the production operator channel and enforces the
// structural TTY gate. The gate fires before the store is opened and before
// any remote call, so a non-TTY invocation makes zero remote calls and
// changes no state.
func newInteractive(name, verb string) (*interactive, error) {
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return nil, fmt.Errorf("%s is interactive-only: run it yourself in a terminal; agent shells cannot %s (both stdin and stdout must be terminals)", name, verb)
	}
	return &interactive{in: bufio.NewReader(os.Stdin), out: os.Stderr}, nil
}

func (t *interactive) confirm(prompt, expect string) error {
	fmt.Fprintf(t.out, "%s\nType %q to confirm, anything else (or Ctrl-D) to abort: ", prompt, expect)
	line, err := t.in.ReadString('\n')
	if strings.TrimSpace(line) == "" {
		if err != nil {
			return fmt.Errorf("aborted: no confirmation was typed; nothing was changed")
		}
		return fmt.Errorf("aborted: the confirmation was left empty; nothing was changed")
	}
	if strings.TrimSpace(line) != expect {
		return fmt.Errorf("aborted: the typed confirmation does not match %q; nothing was changed", expect)
	}
	return nil
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// TestServeCLIProtocol drives the internal `timesheet serve` command as a
// child process over real stdin/stdout JSONL: the status handshake, a
// migration_preview routed to the migration op (disabled here, so it must
// answer migration_disabled on the stream), and close with exit 0.
func TestServeCLIProtocol(t *testing.T) {
	if os.Getenv("TIMESHEET_SERVE_CLI_CHILD") == "1" {
		separator := slices.Index(os.Args, "--")
		if separator < 0 {
			t.Fatal("missing CLI arguments")
		}
		os.Args = append([]string{"timesheet"}, os.Args[separator+1:]...)
		main()
		os.Exit(0)
	}

	home := t.TempDir()
	configPath := filepath.Join(home, "timesheet.json")
	envPath := filepath.Join(home, ".env.local")
	config := `{"timezone":"UTC","state_path":` + jsonString(t, filepath.Join(home, "state.json")) +
		`,"projects":[{"id":"P1","default_classification":"DEV"}]}`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	serve := func(stdin string, args ...string) (string, string, error) {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable,
			append([]string{"-test.run=^TestServeCLIProtocol$", "--", "serve"}, args...)...)
		cmd.Dir = home
		cmd.Env = []string{"TIMESHEET_SERVE_CLI_CHILD=1", "HOME=" + home, "TMPDIR=" + home, "XDG_CONFIG_HOME=" + home}
		cmd.Stdin = strings.NewReader(stdin)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	requests := strings.Join([]string{
		`{"id":"h1","op":"status"}`,
		`{"id":"m1","op":"migration_preview","dates":["2026-10-20"]}`,
		`{"id":"c1","op":"close"}`,
		`{"id":"after","op":"status"}`,
	}, "\n") + "\n"
	stdout, stderr, err := serve(requests, "--config", configPath, "--env-file", envPath)
	if err != nil {
		t.Fatalf("serve: %v; stderr=%s", err, stderr)
	}

	var lines []map[string]json.RawMessage
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		var line map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("stdout line is not JSON (stdout must carry only protocol): %q", scanner.Text())
		}
		lines = append(lines, line)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d response lines, want 3 (nothing after close): %s", len(lines), stdout)
	}

	var status struct {
		ID     string                 `json:"id"`
		OK     bool                   `json:"ok"`
		Result timesheet.StatusResult `json:"result"`
	}
	mustDecode(t, lines[0], &status)
	if status.ID != "h1" || !status.OK || status.Result.ProtocolSchema != timesheet.ProtocolSchema ||
		status.Result.AppVersion != timesheet.AppVersion {
		t.Fatalf("handshake: %+v", status)
	}

	var preview timesheet.Response
	mustDecode(t, lines[1], &preview)
	if preview.ID != "m1" || preview.OK || preview.Error == nil || preview.Error.Code != timesheet.CodeMigrationDisabled {
		t.Fatalf("migration_preview with migration disabled: %+v", preview)
	}

	var closed timesheet.Response
	mustDecode(t, lines[2], &closed)
	if closed.ID != "c1" || !closed.OK {
		t.Fatalf("close: %+v", closed)
	}

	// Missing required flags exit 1 before any protocol output.
	stdout, _, err = serve("", "--config", configPath)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || stdout != "" {
		t.Fatalf("serve without --env-file: err=%v stdout=%q, want exit 1 and no output", err, stdout)
	}
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustDecode(t *testing.T, line map[string]json.RawMessage, v any) {
	t.Helper()
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

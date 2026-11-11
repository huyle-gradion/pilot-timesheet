package timesheet

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// serveClient speaks the serve JSONL protocol to a Session.Serve running on
// the other end of two pipes: one request line in, one response line out.
type serveClient struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Scanner
	done chan error
}

func startServe(t *testing.T, s *Session) *serveClient {
	t.Helper()
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.Serve(reqR, respW)
		_ = respW.Close()
		done <- err
	}()
	t.Cleanup(func() { _ = reqW.Close(); _ = respR.Close() })
	return &serveClient{t: t, in: reqW, out: bufio.NewScanner(respR), done: done}
}

// raw writes one line verbatim and decodes exactly one response line.
func (c *serveClient) raw(line string) (Response, json.RawMessage) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatalf("write request: %v", err)
	}
	if !c.out.Scan() {
		c.t.Fatalf("no response line for %s: %v", line, c.out.Err())
	}
	var envelope struct {
		Response
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(c.out.Bytes(), &envelope); err != nil {
		c.t.Fatalf("response is not one JSON object per line: %q: %v", c.out.Bytes(), err)
	}
	return envelope.Response, envelope.Result
}

func (c *serveClient) send(req Request, result any) Response {
	c.t.Helper()
	line, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, raw := c.raw(string(line))
	if resp.ID != req.ID {
		c.t.Fatalf("response id %q does not echo request id %q", resp.ID, req.ID)
	}
	if resp.OK && result != nil {
		if err := json.Unmarshal(raw, result); err != nil {
			c.t.Fatalf("decode %s result: %v", req.Op, err)
		}
	}
	return resp
}

// TestServeJSONLMigrationEndToEnd drives the internal serve protocol the way
// `timesheet serve` wires it (config and env file loaded from disk, store
// opened, Session.Serve over a byte stream): the status handshake, the
// migration_preview / migration_apply pair that is serve's only remaining
// consumer, a malformed line, and close.
func TestServeJSONLMigrationEndToEnd(t *testing.T) {
	fixture, _, dir := migrationFixture(t)
	cfgBytes, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "timesheet.json")
	envPath := filepath.Join(dir, ".env.local")
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	fileEnv, err := LoadEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	c := startServe(t, NewSession(cfg, MergeEnv(fileEnv), store))

	// Handshake: status names the protocol identity a client negotiates on.
	var status StatusResult
	requireOK(t, c.send(Request{ID: "h1", Op: "status"}, &status))
	if status.ProtocolSchema != ProtocolSchema || status.AppVersion != AppVersion {
		t.Fatalf("handshake identity %d/%q, want %d/%q",
			status.ProtocolSchema, status.AppVersion, ProtocolSchema, AppVersion)
	}
	if status.MigrationReceipts != 0 {
		t.Fatalf("fresh store reports %d migration receipts", status.MigrationReceipts)
	}

	// A malformed line is answered on the stream and the session survives.
	if resp, _ := c.raw("{not json"); resp.OK || resp.Error == nil || resp.Error.Code != CodeBadRequest {
		t.Fatalf("malformed line: %+v", resp)
	}
	requireCode(t, c.send(Request{ID: "x1", Op: "no_such_op"}, nil), CodeUnknownOp)

	var preview MigrationPreview
	requireOK(t, c.send(Request{ID: "m1", Op: "migration_preview", Dates: []string{"2026-10-20"}}, &preview))
	if !preview.Ready || preview.MigrationID == "" || preview.NoChange {
		t.Fatalf("preview over the wire not ready: %+v", preview)
	}
	if len(preview.Dates) != 1 || preview.Dates[0] != "2026-10-20" {
		t.Fatalf("preview dates: %v", preview.Dates)
	}

	requireCode(t, c.send(Request{ID: "m2", Op: "migration_apply", MigrationID: "bogus"}, nil), CodeMigrationStale)

	var receipt MigrationApplyReceipt
	requireOK(t, c.send(Request{ID: "m3", Op: "migration_apply", MigrationID: preview.MigrationID}, &receipt))
	if receipt.Status != MigrationApplied || receipt.ReceiptID != preview.MigrationID {
		t.Fatalf("apply receipt over the wire: %+v", receipt)
	}

	requireOK(t, c.send(Request{ID: "h2", Op: "status"}, &status))
	if status.MigrationReceipts != 1 {
		t.Fatalf("status after apply reports %d migration receipts, want 1", status.MigrationReceipts)
	}

	var closed closeResult
	requireOK(t, c.send(Request{ID: "c1", Op: "close"}, &closed))
	if !closed.Closed {
		t.Fatal("close did not report closed")
	}
	if err := <-c.done; err != nil {
		t.Fatalf("Serve after close: %v", err)
	}
	if c.out.Scan() {
		t.Fatalf("Serve wrote past close: %q", c.out.Bytes())
	}
}

// TestServeEOFEndsCleanly pins that a client hanging up without close is a
// normal end of session, not an error.
func TestServeEOFEndsCleanly(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	c := startServe(t, NewSession(testConfig(store.path), map[string]string{}, store))
	requireOK(t, c.send(Request{ID: "1", Op: "status"}, nil))
	_ = c.in.Close()
	if err := <-c.done; err != nil {
		t.Fatalf("Serve at EOF: %v", err)
	}
}

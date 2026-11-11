package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestInitialize_CapturesSessionIDAndSendsNotification(t *testing.T) {
	f := newFakeMCP()
	sess, _ := newServerSession(t, f)

	info, err := sess.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "fake" || info.Version != "1" || info.ProtocolVersion != ProtocolVersion {
		t.Errorf("unexpected server info: %+v", info)
	}
	if sess.SessionID() != "sess-123" {
		t.Errorf("session id = %q, want sess-123", sess.SessionID())
	}

	reqs := f.recorded()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (initialize + notifications/initialized)", len(reqs))
	}
	if reqs[0].Method != "initialize" || reqs[0].SessionID != "" {
		t.Errorf("initialize request wrong: %+v", reqs[0])
	}
	if reqs[1].Method != "notifications/initialized" {
		t.Errorf("second request = %q, want notifications/initialized", reqs[1].Method)
	}
	if reqs[1].SessionID != "sess-123" {
		t.Errorf("post-init notification missing session header: %+v", reqs[1])
	}
	if reqs[1].HasID {
		t.Error("notifications/initialized must be a JSON-RPC notification (no id)")
	}
	if reqs[0].Auth != "Bearer test-token" {
		t.Errorf("authorization header = %q", reqs[0].Auth)
	}
}

func TestListTools_ParsedAndCarriesSessionHeader(t *testing.T) {
	f := newFakeMCP()
	f.tools = []ToolDecl{{Name: "a", Description: "first"}, {Name: "b"}}
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	tools, err := sess.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "a" || tools[1].Name != "b" {
		t.Errorf("tools = %+v", tools)
	}

	reqs := f.recorded()
	last := reqs[len(reqs)-1]
	if last.Method != "tools/list" || last.SessionID != "sess-123" {
		t.Errorf("tools/list request wrong: %+v", last)
	}
}

func TestListTools_CapExceeded(t *testing.T) {
	f := newFakeMCP()
	f.tools = make([]ToolDecl, MaxToolsCount+1)
	for i := range f.tools {
		f.tools[i] = ToolDecl{Name: "t"}
	}
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	_, err := sess.ListTools(context.Background())
	if !IsKind(err, ErrSchema) || StableCodeOf(err) != CodeTooManyTools {
		t.Fatalf("expected ErrSchema/%s, got %v", CodeTooManyTools, err)
	}
}

func TestCallBeforeInit_Uninitialized(t *testing.T) {
	f := newFakeMCP()
	sess, _ := newServerSession(t, f)

	if _, err := sess.ListTools(context.Background()); !IsUninitialized(err) {
		t.Errorf("ListTools before init = %v, want ErrUninitialized", err)
	}
	if _, err := sess.CallTool(context.Background(), "x", nil); !IsUninitialized(err) {
		t.Errorf("CallTool before init = %v, want ErrUninitialized", err)
	}
	if reqs := f.recorded(); len(reqs) != 0 {
		t.Errorf("pre-init calls must send ZERO requests, got %d: %+v", len(reqs), reqs)
	}
}

func TestJSONRPCErrorMessage_RedactBeforeBound(t *testing.T) {
	const token = "S3CR3T-TOKEN-DO-NOT-LEAK"
	tokenStraddlesTheCap := maxErrorSnippetBytes - len(token)/2
	msg := strings.Repeat("A", tokenStraddlesTheCap) + token

	f := newFakeMCP()
	f.rpcErr = &rpcError{Code: -32000, Message: msg}
	sess, _ := newServerSession(t, f)
	sess.cfg.Token = token
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	_, err := sess.CallTool(context.Background(), "t", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("full token leaked: %v", err)
	}
	for k := 4; k <= len(token); k++ {
		if strings.Contains(err.Error(), token[:k]) {
			t.Errorf("token prefix %q leaked: %v", token[:k], err)
		}
	}
}

func TestProtocolVersionMismatch_Bounded(t *testing.T) {
	f := newFakeMCP()
	f.protocolVersion = strings.Repeat("v", 1000)
	sess, _ := newServerSession(t, f)

	_, err := sess.Initialize(context.Background())
	if !IsKind(err, ErrProtocol) || StableCodeOf(err) != CodeProtocolVersion {
		t.Fatalf("expected ErrProtocol/%s, got %v", CodeProtocolVersion, err)
	}
	if len(err.Error()) > 400 {
		t.Errorf("mismatch message not bounded: len=%d", len(err.Error()))
	}
}

func TestPostInitOmitsSessionHeaderWhenUnassigned(t *testing.T) {
	f := newFakeMCP()
	f.omitSessionID = true
	sess, _ := newServerSession(t, f)

	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if sess.SessionID() != "" {
		t.Errorf("expected no captured session id, got %q", sess.SessionID())
	}
	if _, err := sess.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	reqs := f.recorded()
	last := reqs[len(reqs)-1]
	if last.Method != "tools/list" || last.SessionID != "" {
		t.Errorf("expected no session header on post-init request, got %+v", last)
	}
}

func Test404SessionExpired(t *testing.T) {
	f := newFakeMCP()
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	f.mu.Lock()
	f.forceStatus = http.StatusNotFound
	f.mu.Unlock()

	_, err := sess.ListTools(context.Background())
	if !IsKind(err, ErrHTTPStatus) || StableCodeOf(err) != CodeSessionExpired {
		t.Fatalf("expected ErrHTTPStatus/%s, got %v", CodeSessionExpired, err)
	}
}

func TestResponseIDMismatch(t *testing.T) {
	f := newFakeMCP()
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	f.mu.Lock()
	f.forceBody = `{"jsonrpc":"2.0","id":999,"result":{"tools":[]}}`
	f.mu.Unlock()

	_, err := sess.ListTools(context.Background())
	if !IsKind(err, ErrProtocol) || StableCodeOf(err) != CodeProtocol {
		t.Fatalf("expected ErrProtocol/%s, got %v", CodeProtocol, err)
	}
}

func TestResponseJSONRPCVersionInvalid(t *testing.T) {
	f := newFakeMCP()
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	f.mu.Lock()
	f.forceBody = `{"jsonrpc":"1.0","id":2,"result":{"tools":[]}}`
	f.mu.Unlock()

	_, err := sess.ListTools(context.Background())
	if !IsKind(err, ErrProtocol) || StableCodeOf(err) != CodeProtocol {
		t.Fatalf("expected ErrProtocol/%s, got %v", CodeProtocol, err)
	}
}

func TestProtocolVersionMismatch(t *testing.T) {
	f := newFakeMCP()
	f.protocolVersion = "1999-01-01"
	sess, _ := newServerSession(t, f)

	_, err := sess.Initialize(context.Background())
	if !IsKind(err, ErrProtocol) || StableCodeOf(err) != CodeProtocolVersion {
		t.Fatalf("expected ErrProtocol/%s, got %v", CodeProtocolVersion, err)
	}
}

func TestCallTool_ToolLevelError(t *testing.T) {
	f := newFakeMCP()
	f.callResultRaw = json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"boom"}]}`)
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := sess.CallTool(context.Background(), "timesheet__log_time", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Error("tool-level isError not surfaced")
	}
	if res.Text != "boom" {
		t.Errorf("text = %q, want boom", res.Text)
	}
}

func TestCallTool_JSONRPCError(t *testing.T) {
	f := newFakeMCP()
	f.rpcErr = &rpcError{Code: -32601, Message: "method not found"}
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	_, err := sess.CallTool(context.Background(), "nope", nil)
	if !IsKind(err, ErrProtocol) {
		t.Fatalf("expected ErrProtocol, got %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != -32601 {
		t.Errorf("JSON-RPC code not surfaced: %v", err)
	}
}

func TestCallTool_StructuredPreferred(t *testing.T) {
	f := newFakeMCP()
	f.callResultRaw = json.RawMessage(`{"structuredContent":{"total":3},"content":[{"type":"text","text":"ignored"}]}`)
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := sess.CallTool(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Structured) == 0 {
		t.Error("structuredContent not surfaced")
	}
	if res.Text != "" {
		t.Errorf("text should be empty when structuredContent is present, got %q", res.Text)
	}
}

func TestCallTool_ContentCapturedAlongsideStructured(t *testing.T) {
	f := newFakeMCP()
	f.callResultRaw = json.RawMessage(`{"structuredContent":{"resources":[{"id":"u1"}]},"content":[{"type":"text","text":"- Name (CODE) [internal project] · u1"}]}`)
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := sess.CallTool(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Structured) == 0 {
		t.Error("structuredContent not surfaced")
	}
	if res.Content != "- Name (CODE) [internal project] · u1" {
		t.Errorf("Content = %q, want the text block captured alongside structuredContent", res.Content)
	}
	if res.Text != "" {
		t.Errorf("Text must stay empty when structuredContent is present, got %q", res.Text)
	}
}

func TestCallTool_ContentIsBounded(t *testing.T) {
	big := strings.Repeat("x", MaxTextBlockBytes+100)
	f := newFakeMCP()
	payload, _ := json.Marshal(map[string]any{
		"structuredContent": map[string]any{"ok": true},
		"content":           []map[string]any{{"type": "text", "text": big}},
	})
	f.callResultRaw = payload
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := sess.CallTool(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) > MaxTextBlockBytes {
		t.Errorf("Content length = %d, want <= %d", len(res.Content), MaxTextBlockBytes)
	}
	if len(res.Content) == 0 {
		t.Error("Content must not be empty")
	}
}

func TestCallTool_TextFallback(t *testing.T) {
	f := newFakeMCP()
	f.callResultRaw = json.RawMessage(`{"content":[{"type":"text","text":"hello"}]}`)
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := sess.CallTool(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text != "hello" || len(res.Structured) != 0 {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestCallTool_NeitherOutput(t *testing.T) {
	f := newFakeMCP()
	f.callResultRaw = json.RawMessage(`{"content":[]}`)
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	_, err := sess.CallTool(context.Background(), "t", nil)
	if !IsKind(err, ErrSchema) || StableCodeOf(err) != CodeNoToolOutput {
		t.Fatalf("expected ErrSchema/%s, got %v", CodeNoToolOutput, err)
	}
}

func TestResponseTooLarge(t *testing.T) {
	f := newFakeMCP()
	sess, _ := newServerSession(t, f)
	sess.cfg.MaxResponseBytes = 200
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	f.mu.Lock()
	f.forceBody = strings.Repeat("x", 1000)
	f.mu.Unlock()

	_, err := sess.ListTools(context.Background())
	if !IsKind(err, ErrSchema) || StableCodeOf(err) != CodeResponseTooLarge {
		t.Fatalf("expected ErrSchema/%s, got %v", CodeResponseTooLarge, err)
	}
}

func TestMalformedJSON(t *testing.T) {
	f := newFakeMCP()
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	f.mu.Lock()
	f.forceBody = "not valid json"
	f.mu.Unlock()

	_, err := sess.ListTools(context.Background())
	if !IsKind(err, ErrSchema) || StableCodeOf(err) != CodeSchema {
		t.Fatalf("expected ErrSchema/%s, got %v", CodeSchema, err)
	}
}

func TestSSEContentTypeFailsClosed(t *testing.T) {
	f := newFakeMCP()
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	f.mu.Lock()
	f.forceCT = "text/event-stream"
	f.forceBody = "data: {}\n\n"
	f.mu.Unlock()

	_, err := sess.ListTools(context.Background())
	if !IsKind(err, ErrProtocol) || StableCodeOf(err) != CodeSSEUnsupported {
		t.Fatalf("expected ErrProtocol/%s, got %v", CodeSSEUnsupported, err)
	}
}

func TestTimeoutIsTyped(t *testing.T) {
	tr := &fakeTransport{fn: func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}}
	sess := newTestSession(t, Config{Endpoint: "http://example.invalid", Token: "t", Timeout: 20 * time.Millisecond, Transport: tr})

	_, err := sess.Initialize(context.Background())
	if !IsKind(err, ErrTransport) || StableCodeOf(err) != CodeTimeout {
		t.Fatalf("expected ErrTransport/%s, got %v", CodeTimeout, err)
	}
}

func TestTransportResetIsTyped(t *testing.T) {
	tr := &fakeTransport{fn: func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset by peer")
	}}
	sess := newTestSession(t, Config{Endpoint: "http://example.invalid", Token: "t", Transport: tr})

	_, err := sess.Initialize(context.Background())
	if !IsKind(err, ErrTransport) || StableCodeOf(err) != CodeTransport {
		t.Fatalf("expected ErrTransport/%s, got %v", CodeTransport, err)
	}
}

func TestCallTool_ExactlyOneAttempt(t *testing.T) {
	tr := methodAwareTransport(func(method string, _ *http.Request) (*http.Response, error) {
		return resp(http.StatusInternalServerError, "application/json", "boom"), nil
	})
	sess := newTestSession(t, Config{Endpoint: "http://example.invalid", Token: "t", Transport: tr})
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if _, err := sess.CallTool(context.Background(), "t", nil); err == nil {
		t.Fatal("expected a 5xx error")
	}
	if got := tr.countMethod("tools/call"); got != 1 {
		t.Errorf("tools/call attempts = %d, want exactly 1 (no retry)", got)
	}
}

func TestTokenNeverLeaks(t *testing.T) {
	const token = "S3CR3T-TOKEN-DO-NOT-LEAK"

	tr := methodAwareTransport(func(string, *http.Request) (*http.Response, error) {
		return resp(http.StatusInternalServerError, "application/json", "leaked "+token), nil
	})
	sess := newTestSession(t, Config{Endpoint: "http://example.invalid", Token: token, Transport: tr})
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	_, err := sess.CallTool(context.Background(), "t", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("token leaked into HTTP-status error: %v", err)
	}

	f := newFakeMCP()
	f.rpcErr = &rpcError{Code: -32000, Message: "bad " + token}
	sess2, _ := newServerSession(t, f)
	sess2.cfg.Token = token
	if _, err := sess2.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	_, err = sess2.CallTool(context.Background(), "t", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("token leaked into JSON-RPC error: %v", err)
	}

	cfgData, _ := json.Marshal(Config{Endpoint: "http://example.invalid", Token: token})
	if strings.Contains(string(cfgData), token) {
		t.Errorf("token leaked into serialized Config: %s", cfgData)
	}
	sessData, _ := json.Marshal(sess)
	if strings.Contains(string(sessData), token) {
		t.Errorf("token leaked into serialized Session: %s", sessData)
	}
}

func TestClose_IdempotentAndBlocks(t *testing.T) {
	f := newFakeMCP()
	sess, _ := newServerSession(t, f)
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := sess.ListTools(context.Background()); StableCodeOf(err) != CodeSessionClosed {
		t.Errorf("expected %s after Close, got %v", CodeSessionClosed, err)
	}
}

func TestNewSession_RejectsEmptyEndpoint(t *testing.T) {
	if _, err := NewSession(Config{}); err == nil {
		t.Error("expected an error for an empty endpoint")
	}
}

func TestStatusErrorRedactsBeforeBound(t *testing.T) {
	const token = "S3CR3T-TOKEN-DO-NOT-LEAK"
	body := strings.Repeat("A", maxErrorSnippetBytes-6) + token
	tr := methodAwareTransport(func(string, *http.Request) (*http.Response, error) {
		return resp(http.StatusInternalServerError, "application/json", body), nil
	})
	sess := newTestSession(t, Config{Endpoint: "http://example.invalid", Token: token, Transport: tr})
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	_, err := sess.CallTool(context.Background(), "t", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	for k := 4; k <= len(token); k++ {
		if strings.Contains(err.Error(), token[:k]) {
			t.Errorf("token prefix %q leaked: %v", token[:k], err)
		}
	}
}

func TestProtocolVersionMismatchRedactsToken(t *testing.T) {
	const token = "S3CR3T-TOKEN-DO-NOT-LEAK"
	f := newFakeMCP()
	f.protocolVersion = strings.Repeat("v", maxErrorSnippetBytes-6) + token
	sess, _ := newServerSession(t, f)
	sess.cfg.Token = token

	_, err := sess.Initialize(context.Background())
	if StableCodeOf(err) != CodeProtocolVersion {
		t.Fatalf("expected %s, got %v", CodeProtocolVersion, err)
	}
	for k := 4; k <= len(token); k++ {
		if strings.Contains(err.Error(), token[:k]) {
			t.Errorf("token prefix %q leaked into the version mismatch: %v", token[:k], err)
		}
	}
}

func TestDefaultClientDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	sess := newTestSession(t, Config{Endpoint: origin.URL, Token: "test-token"})
	_, err := sess.Initialize(context.Background())
	if !IsKind(err, ErrHTTPStatus) || StableCodeOf(err) != CodeHTTPStatus {
		t.Fatalf("expected a typed %s error, got %v", CodeHTTPStatus, err)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target was hit %d time(s); the bearer token must not follow a redirect", n)
	}
}

func TestBoundStringKeepsRuneBoundaries(t *testing.T) {
	const s = "ab€d"
	for max := 1; max <= len(s); max++ {
		got := boundString(s, max)
		if !utf8.ValidString(got) || !strings.HasPrefix(s, got) || len(got) > max {
			t.Errorf("boundString(%q, %d) = %q, want a valid UTF-8 prefix of at most %d bytes", s, max, got, max)
		}
	}
	for _, max := range []int{3, 4} {
		if got := boundString(s, max); got != "ab" {
			t.Errorf("boundString(%q, %d) = %q, want the split rune dropped", s, max, got)
		}
	}
}

func TestBoundString(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"ascii fits", "abc", 3, "abc"},
		{"ascii cut", "abcdef", 4, "abcd"},
		{"2-byte rune ends at max", "abé" + "x", 4, "abé"},
		{"3-byte rune ends at max", "a€" + "x", 4, "a€"},
		{"4-byte rune ends at max", "a😀" + "x", 5, "a😀"},
		{"2-byte rune split by max", "abé", 3, "ab"},
		{"3-byte rune split by max", "a€x", 3, "a"},
		{"4-byte rune split by max", "a😀x", 4, "a"},
		{"rune at start split", "€abc", 2, ""},
		{"invalid byte before max stops the prefix", "ab\xffcdef", 5, "ab"},
		{"truncated lead before max is dropped", "abc\xe2x", 4, "abc"},
		{"invalid input that fits is unchanged", "ab\xff", 3, "ab\xff"},
		{"encoded replacement rune is kept", "a�x", 4, "a�"},
		{"zero max is unbounded", "abc€", 0, "abc€"},
		{"negative max is unbounded", "abc€", -1, "abc€"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boundString(tt.s, tt.max)
			if got != tt.want {
				t.Fatalf("boundString(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
			if tt.max > 0 && len(tt.s) > tt.max && !utf8.ValidString(got) {
				t.Fatalf("boundString(%q, %d) = %q, want valid UTF-8 after a cut", tt.s, tt.max, got)
			}
		})
	}
}

func TestRequestsAcceptOnlyJSON(t *testing.T) {
	tr := methodAwareTransport(func(string, *http.Request) (*http.Response, error) {
		return resp(http.StatusOK, "application/json", `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`), nil
	})
	var accepts []string
	inner := tr.fn
	tr.fn = func(r *http.Request) (*http.Response, error) {
		accepts = append(accepts, r.Header.Get("Accept"))
		return inner(r)
	}
	sess := newTestSession(t, Config{Endpoint: "http://example.invalid", Transport: tr})
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := sess.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(accepts) != 3 {
		t.Fatalf("recorded %d requests, want initialize, initialized and tools/list", len(accepts))
	}
	for _, a := range accepts {
		if a != "application/json" {
			t.Errorf("Accept = %q, want only application/json: SSE responses are not parsed", a)
		}
	}
}

func TestConcurrentCallsAreSerializedWithDistinctIDs(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32
	var mu sync.Mutex
	seen := map[int64]bool{}
	tr := methodAwareTransport(func(method string, r *http.Request) (*http.Response, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		var req rpcRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil || req.ID == nil {
			return nil, errors.New("request without an id")
		}
		mu.Lock()
		dup := seen[*req.ID]
		seen[*req.ID] = true
		mu.Unlock()
		if dup {
			t.Errorf("request id %d sent twice", *req.ID)
		}
		time.Sleep(2 * time.Millisecond)
		result := `{"structuredContent":{"ok":true}}`
		if method == "tools/list" {
			result = `{"tools":[]}`
		}
		return resp(http.StatusOK, "application/json", fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, *req.ID, result)), nil
	})
	sess := newTestSession(t, Config{Endpoint: "http://example.invalid", Transport: tr})
	if _, err := sess.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = sess.CallTool(context.Background(), "t", nil)
			} else {
				_, err = sess.ListTools(context.Background())
			}
			if err != nil {
				t.Errorf("call %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if m := maxInFlight.Load(); m != 1 {
		t.Errorf("max requests in flight = %d, want 1", m)
	}
}

func TestCallResultRefusesToMarshal(t *testing.T) {
	if data, err := json.Marshal(CallResult{IsError: true, Text: "x"}); err == nil {
		t.Fatalf("CallResult marshaled to %s; it is not the MCP wire shape", data)
	}
}

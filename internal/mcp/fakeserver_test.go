package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recordedRequest struct {
	Method    string
	SessionID string
	Auth      string
	HasID     bool
}

type fakeMCP struct {
	mu              sync.Mutex
	requests        []recordedRequest
	sessionID       string
	protocolVersion string
	tools           []ToolDecl
	callResultRaw   json.RawMessage
	rpcErr          *rpcError
	forceStatus     int
	forceCT         string
	forceBody       string
	omitSessionID   bool
}

func newFakeMCP() *fakeMCP {
	return &fakeMCP{sessionID: "sess-123", protocolVersion: ProtocolVersion}
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req rpcRequest
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method:    req.Method,
		SessionID: r.Header.Get("Mcp-Session-Id"),
		Auth:      r.Header.Get("Authorization"),
		HasID:     req.ID != nil,
	})
	status := f.forceStatus
	ct := f.forceCT
	rawBody := f.forceBody
	omit := f.omitSessionID
	protocolVersion := f.protocolVersion
	tools := f.tools
	callRaw := f.callResultRaw
	rpcErr := f.rpcErr
	sid := f.sessionID
	f.mu.Unlock()

	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if !omit && sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("forced error body"))
		return
	}
	if rawBody != "" {
		if ct == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = w.Write([]byte(rawBody))
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "initialize":
		writeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			"capabilities":    map[string]any{},
		})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		if rpcErr != nil {
			writeRPCError(w, req.ID, rpcErr)
			return
		}
		writeRPCResultRaw(w, req.ID, callRaw)
	default:
		writeRPCResult(w, req.ID, map[string]any{})
	}
}

func writeRPCResult(w http.ResponseWriter, id *int64, result any) {
	rb, _ := json.Marshal(result)
	writeRPCResultRaw(w, id, rb)
}

func writeRPCResultRaw(w http.ResponseWriter, id *int64, raw json.RawMessage) {
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: raw})
}

func writeRPCError(w http.ResponseWriter, id *int64, e *rpcError) {
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: e})
}

type fakeTransport struct {
	mu      sync.Mutex
	methods []string
	fn      func(*http.Request) (*http.Response, error)
}

func (t *fakeTransport) Do(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.methods = append(t.methods, requestMethod(r))
	t.mu.Unlock()
	return t.fn(r)
}

func (t *fakeTransport) calls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.methods)
}

func (t *fakeTransport) countMethod(method string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, m := range t.methods {
		if m == method {
			n++
		}
	}
	return n
}

func requestMethod(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return req.Method
}

func resp(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func methodAwareTransport(fn func(method string, r *http.Request) (*http.Response, error)) *fakeTransport {
	return &fakeTransport{fn: func(r *http.Request) (*http.Response, error) {
		method := requestMethod(r)
		switch method {
		case "initialize":
			return resp(200, "application/json", `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"`+ProtocolVersion+`","serverInfo":{"name":"x","version":"1"}}}`), nil
		case "notifications/initialized":
			return resp(http.StatusAccepted, "application/json", ""), nil
		default:
			return fn(method, r)
		}
	}}
}

func newTestSession(t *testing.T, cfg Config) *Session {
	t.Helper()
	if cfg.ClientName == "" {
		cfg.ClientName = "test-client"
	}
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = "1"
	}
	sess, err := NewSession(cfg)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return sess
}

func newServerSession(t *testing.T, f *fakeMCP) (*Session, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newTestSession(t, Config{Endpoint: srv.URL, Token: "test-token"}), srv
}

func (f *fakeMCP) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

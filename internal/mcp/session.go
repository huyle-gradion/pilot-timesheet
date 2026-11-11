package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Transport interface {
	Do(req *http.Request) (*http.Response, error)
}

type Config struct {
	Endpoint         string
	Token            string        `json:"-"`
	HTTPClient       *http.Client  `json:"-"`
	Timeout          time.Duration `json:"-"`
	MaxResponseBytes int64
	ClientName       string
	ClientVersion    string
	ProtocolVersion  string
	Transport        Transport `json:"-"`
}

// Session is a single-in-flight MCP JSON-RPC session.
type Session struct {
	cfg       Config
	transport Transport

	mu          sync.Mutex
	sessionID   string
	nextID      int64
	initialized bool
	closed      bool
	serverInfo  ServerInfo
}

// NewSession validates cfg and returns a Session. It performs NO IO.
func NewSession(cfg Config) (*Session, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, newError(ErrTransport, CodeTransport, "MCP endpoint must not be empty")
	}
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultRequestTimeout
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if strings.TrimSpace(cfg.ClientName) == "" {
		cfg.ClientName = DefaultClientName
	}
	if strings.TrimSpace(cfg.ClientVersion) == "" {
		cfg.ClientVersion = DefaultClientVersion
	}
	if strings.TrimSpace(cfg.ProtocolVersion) == "" {
		cfg.ProtocolVersion = ProtocolVersion
	}
	return &Session{cfg: cfg, transport: cfg.resolveTransport()}, nil
}

func (cfg Config) resolveTransport() Transport {
	if cfg.Transport != nil {
		return cfg.Transport
	}
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient
	}
	return &http.Client{
		Timeout: cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// SessionID returns the captured Mcp-Session-Id, or "".
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Initialize performs the MCP handshake: initialize (capturing Mcp-Session-Id),
// then notifications/initialized. It is idempotent.
func (s *Session) Initialize(ctx context.Context) (ServerInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ServerInfo{}, newError(ErrTransport, CodeSessionClosed, "MCP session is closed")
	}
	if s.initialized {
		return s.serverInfo, nil
	}

	params := map[string]any{
		"protocolVersion": s.cfg.ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    s.cfg.ClientName,
			"version": s.cfg.ClientVersion,
		},
	}
	result, err := s.doRPC(ctx, "initialize", params, false)
	if err != nil {
		return ServerInfo{}, err
	}

	info, err := parseServerInfo(result)
	if err != nil {
		return ServerInfo{}, err
	}
	if info.ProtocolVersion != "" && info.ProtocolVersion != s.cfg.ProtocolVersion {
		return ServerInfo{}, &Error{
			Kind:       ErrProtocol,
			StableCode: CodeProtocolVersion,
			Message: fmt.Sprintf("MCP protocol version mismatch: server %q, client %q",
				boundString(redact(info.ProtocolVersion, s.cfg.Token), maxErrorSnippetBytes), s.cfg.ProtocolVersion),
		}
	}

	if _, err := s.doRPC(ctx, "notifications/initialized", map[string]any{}, true); err != nil {
		return ServerInfo{}, err
	}

	s.initialized = true
	s.serverInfo = info
	return info, nil
}

// ListTools fetches the declared tool list. It fails closed before Initialize.
func (s *Session) ListTools(ctx context.Context) ([]ToolDecl, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.guardInitialized(); err != nil {
		return nil, err
	}
	result, err := s.doRPC(ctx, "tools/list", map[string]any{}, false)
	if err != nil {
		return nil, err
	}
	return parseToolDecls(result)
}

// CallTool performs a single tools/call request and surfaces a tool-level error
// inside a 200 via CallResult.IsError. It performs NO application-level retry:
// this layer dispatches exactly one request. (Go's HTTP transport may re-attempt
// only when zero request bytes were written, which the server cannot observe as
// a duplicate.)
func (s *Session) CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.guardInitialized(); err != nil {
		return CallResult{}, err
	}
	if strings.TrimSpace(name) == "" {
		return CallResult{}, newError(ErrSchema, CodeSchema, "MCP tool name must not be empty")
	}
	if args == nil {
		args = map[string]any{}
	}

	result, err := s.doRPC(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, false)
	if err != nil {
		return CallResult{}, err
	}
	return parseCallResult(result, s.cfg.MaxResponseBytes)
}

// Close releases the session. It is idempotent and performs no IO.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *Session) guardInitialized() error {
	if s.closed {
		return newError(ErrTransport, CodeSessionClosed, "MCP session is closed")
	}
	if !s.initialized {
		return newError(ErrUninitialized, CodeUninitialized, "MCP session not initialized: call Initialize first")
	}
	return nil
}

// doRPC performs one JSON-RPC HTTP POST. Callers MUST hold s.mu. It never retries.
func (s *Session) doRPC(ctx context.Context, method string, params any, notification bool) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, newError(ErrTransport, CodeContextCanceled, "MCP context canceled before dispatch")
	}

	var id *int64
	if !notification {
		s.nextID++
		v := s.nextID
		id = &v
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, newError(ErrSchema, CodeSchema, "failed to encode JSON-RPC request")
	}

	reqCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, s.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, newError(ErrTransport, CodeTransport, "failed to construct MCP HTTP request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if s.cfg.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	if s.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", s.sessionID)
	}

	resp, err := s.transport.Do(httpReq)
	if err != nil {
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return nil, newError(ErrTransport, CodeTimeout, "MCP request exceeded the bounded timeout")
		}
		if errors.Is(reqCtx.Err(), context.Canceled) {
			return nil, newError(ErrTransport, CodeContextCanceled, "MCP request canceled")
		}
		return nil, newError(ErrTransport, CodeTransport, "MCP network transport failure")
	}
	defer func() { _ = resp.Body.Close() }()

	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		s.sessionID = sid
	}

	if isSSE(resp.Header.Get("Content-Type")) {
		return nil, newError(ErrProtocol, CodeSSEUnsupported, "MCP response used unsupported text/event-stream framing")
	}

	body, err := s.readBounded(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		stable := CodeHTTPStatus
		if resp.StatusCode == http.StatusNotFound && method != "initialize" {
			stable = CodeSessionExpired
		}
		return nil, &Error{
			Kind:       ErrHTTPStatus,
			StatusCode: resp.StatusCode,
			StableCode: stable,
			Message:    s.statusMessage(resp.StatusCode, body),
		}
	}

	if notification {
		return nil, nil
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, newError(ErrSchema, CodeSchema, "MCP response body was empty")
	}
	var rpc rpcResponse
	if err := json.Unmarshal(trimmed, &rpc); err != nil {
		return nil, newError(ErrSchema, CodeSchema, "MCP response was not valid JSON")
	}
	if rpc.JSONRPC != "2.0" {
		return nil, newError(ErrProtocol, CodeProtocol, "MCP response had an invalid jsonrpc version")
	}
	if id == nil || rpc.ID == nil || *rpc.ID != *id {
		return nil, newError(ErrProtocol, CodeProtocol, "MCP response id did not match the request id")
	}
	if rpc.Error != nil {
		return nil, &Error{
			Kind:       ErrProtocol,
			Code:       rpc.Error.Code,
			StableCode: CodeProtocol,
			Message:    boundString(redact(rpc.Error.Message, s.cfg.Token), maxErrorSnippetBytes),
		}
	}
	return rpc.Result, nil
}

func (s *Session) readBounded(body io.Reader) ([]byte, error) {
	max := s.cfg.MaxResponseBytes
	if max <= 0 {
		max = DefaultMaxResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, newError(ErrTransport, CodeTransport, "failed to read MCP response body")
	}
	if int64(len(data)) > max {
		return nil, newError(ErrSchema, CodeResponseTooLarge, "MCP response exceeded the configured byte limit")
	}
	return data, nil
}

func (s *Session) statusMessage(status int, body []byte) string {
	snippet := redact(string(body), s.cfg.Token)
	if len(snippet) > maxErrorSnippetBytes {
		snippet = snippet[:maxErrorSnippetBytes]
	}
	if strings.TrimSpace(snippet) == "" {
		return fmt.Sprintf("MCP HTTP status %d", status)
	}
	return fmt.Sprintf("MCP HTTP status %d: %s", status, snippet)
}

func isSSE(contentType string) bool {
	if contentType == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.Contains(strings.ToLower(contentType), "text/event-stream")
	}
	return strings.EqualFold(mt, "text/event-stream")
}

// redact does not detect encoded or split forms of the token.
func redact(msg, token string) string {
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "[REDACTED]")
}

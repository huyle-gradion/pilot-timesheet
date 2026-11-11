package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type ServerInfo struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	ProtocolVersion string `json:"protocolVersion"`
}

type ToolDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// CallResult is the bounded, NORMALIZED result of a tools/call. IsError surfaces
// the tool-level error inside a successful HTTP response (result.isError).
type CallResult struct {
	IsError    bool
	Structured json.RawMessage
	// Text is the first text block, populated ONLY when structuredContent is
	// absent (the legacy fallback). It is deliberately empty when structuredContent
	// is present, so callers that prefer structured data are unaffected.
	Text string
	// Content is the first text block, captured ALWAYS — including when
	// structuredContent is present. Some live tools return a structured reference
	// envelope PLUS an authoritative plain-text rendering (e.g.
	// timesheet__get_classifications carries Code + Activity Type only in the text
	// block). Content makes that text available without changing Text's fallback
	// semantics.
	Content string
}

// MarshalJSON refuses: a CallResult is not the MCP wire shape.
func (CallResult) MarshalJSON() ([]byte, error) {
	return nil, errors.New("mcp: CallResult is decode-only")
}

type initResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type listToolsResult struct {
	Tools []ToolDecl `json:"tools"`
}

type callToolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []contentBlock  `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseServerInfo(result json.RawMessage) (ServerInfo, error) {
	var ir initResult
	if len(bytes.TrimSpace(result)) > 0 {
		if err := json.Unmarshal(result, &ir); err != nil {
			return ServerInfo{}, newError(ErrSchema, CodeSchema, "MCP initialize result was not valid JSON")
		}
	}
	return ServerInfo{
		Name:            ir.ServerInfo.Name,
		Version:         ir.ServerInfo.Version,
		ProtocolVersion: ir.ProtocolVersion,
	}, nil
}

func parseToolDecls(result json.RawMessage) ([]ToolDecl, error) {
	var lr listToolsResult
	if err := json.Unmarshal(result, &lr); err != nil {
		return nil, newError(ErrSchema, CodeSchema, "MCP tools/list result was not valid JSON")
	}
	if len(lr.Tools) > MaxToolsCount {
		return nil, newError(ErrSchema, CodeTooManyTools, "MCP tools/list exceeded the admitted tool cap")
	}
	return lr.Tools, nil
}

func parseCallResult(result json.RawMessage, maxBytes int64) (CallResult, error) {
	var cr callToolResult
	if err := json.Unmarshal(result, &cr); err != nil {
		return CallResult{}, newError(ErrSchema, CodeSchema, "MCP tools/call result was not valid JSON")
	}

	out := CallResult{IsError: cr.IsError}

	hasStructured := false
	if sc := bytes.TrimSpace(cr.StructuredContent); len(sc) > 0 && !bytes.Equal(sc, []byte("null")) {
		if maxBytes > 0 && int64(len(sc)) > maxBytes {
			return CallResult{}, newError(ErrSchema, CodeResponseTooLarge, "MCP structuredContent exceeded the response limit")
		}
		out.Structured = append(json.RawMessage(nil), sc...)
		hasStructured = true
	}

	for _, block := range cr.Content {
		if block.Type == "text" || block.Type == "" {
			out.Content = boundString(block.Text, MaxTextBlockBytes)
			break
		}
	}
	if !hasStructured {
		out.Text = out.Content
	}
	if !hasStructured && out.Text == "" {
		return CallResult{}, newError(ErrSchema, CodeNoToolOutput, "MCP tool result had neither structuredContent nor a text block")
	}
	return out, nil
}

// boundString returns s unchanged when max <= 0 or s fits in max bytes.
// Otherwise it returns the longest prefix of s of at most max bytes that ends
// on a rune boundary. A cut prefix is always valid UTF-8: it stops before the
// first invalid byte, so it never ends in or carries a broken sequence.
func boundString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	n := 0
	for n < len(s) {
		r, size := utf8.DecodeRuneInString(s[n:])
		if (r == utf8.RuneError && size == 1) || n+size > max {
			break
		}
		n += size
	}
	return s[:n]
}

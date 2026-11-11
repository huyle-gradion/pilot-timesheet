// Package mcp implements a generic MCP (Model Context Protocol) JSON-RPC 2.0
// transport over HTTP POST. It is deliberately narrow and hermetic-friendly:
// stdlib only, no REST fallback, no retries, and fail-closed framing.
package mcp

import "time"

const (
	DefaultRequestTimeout         = 15 * time.Second
	DefaultMaxResponseBytes int64 = 256 * 1024
	// MaxToolsCount caps the number of tools admitted from tools/list.
	MaxToolsCount = 200
	// MaxTextBlockBytes bounds the first text block extracted from a tool result.
	MaxTextBlockBytes    = 64 * 1024
	maxErrorSnippetBytes = 256
)

// ProtocolVersion is the single MCP protocol version this transport speaks.
const ProtocolVersion = "2025-06-18"

const (
	DefaultClientName    = "pilot-timesheet"
	DefaultClientVersion = "1.0.0"
)

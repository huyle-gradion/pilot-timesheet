package mcp

import (
	"errors"
	"fmt"
)

type ErrorKind int

const (
	// ErrTransport is a network/transport failure (connection reset, timeout,
	// context cancellation) with no HTTP response.
	ErrTransport ErrorKind = iota
	// ErrHTTPStatus is a non-2xx HTTP response.
	ErrHTTPStatus
	// ErrProtocol is a JSON-RPC protocol error (an `error` object, SSE framing,
	// or a protocol-version mismatch).
	ErrProtocol
	// ErrSchema is a response that cannot be parsed or violates a bound.
	ErrSchema
	// ErrUninitialized is returned when a request is attempted before Initialize.
	ErrUninitialized
)

func (k ErrorKind) String() string {
	switch k {
	case ErrTransport:
		return "ErrTransport"
	case ErrHTTPStatus:
		return "ErrHTTPStatus"
	case ErrProtocol:
		return "ErrProtocol"
	case ErrSchema:
		return "ErrSchema"
	case ErrUninitialized:
		return "ErrUninitialized"
	default:
		return fmt.Sprintf("ErrorKind(%d)", int(k))
	}
}

// Stable, secret-free error codes. Callers branch on these, never on messages.
const (
	CodeTransport        = "MCP_ERR_TRANSPORT"
	CodeTimeout          = "MCP_ERR_TIMEOUT"
	CodeContextCanceled  = "MCP_ERR_CONTEXT_CANCELED"
	CodeHTTPStatus       = "MCP_ERR_HTTP_STATUS"
	CodeSessionExpired   = "MCP_ERR_SESSION_EXPIRED"
	CodeProtocol         = "MCP_ERR_PROTOCOL"
	CodeProtocolVersion  = "MCP_ERR_PROTOCOL_VERSION"
	CodeSSEUnsupported   = "MCP_ERR_SSE_UNSUPPORTED"
	CodeSchema           = "MCP_ERR_SCHEMA"
	CodeResponseTooLarge = "MCP_ERR_RESPONSE_TOO_LARGE"
	CodeTooManyTools     = "MCP_ERR_TOO_MANY_TOOLS"
	CodeNoToolOutput     = "MCP_ERR_NO_TOOL_OUTPUT"
	CodeUninitialized    = "MCP_ERR_UNINITIALIZED"
	CodeSessionClosed    = "MCP_ERR_SESSION_CLOSED"
)

// Error is a typed, bounded, secret-free MCP error. Message is already
// redacted and bounded at construction; Error() never exposes a secret. It
// carries no wrapped cause, so a raw (potentially secret-bearing) transport
// error can never surface through it.
type Error struct {
	Kind       ErrorKind
	StatusCode int // HTTP status for ErrHTTPStatus, else 0
	Code       int // JSON-RPC error code for ErrProtocol, else 0
	StableCode string
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("[%s] %s", e.StableCode, e.Message)
}

func IsUninitialized(err error) bool {
	return IsKind(err, ErrUninitialized)
}

func IsKind(err error, kind ErrorKind) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind == kind
	}
	return false
}

// StableCodeOf returns the stable code of an MCP error, or "".
func StableCodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.StableCode
	}
	return ""
}

func newError(kind ErrorKind, stableCode, message string) *Error {
	return &Error{Kind: kind, StableCode: stableCode, Message: message}
}

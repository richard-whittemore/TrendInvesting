// Package transport implements the local process boundary between the thin
// Python LEAN adapter and the Go decision engine.
//
// The wire protocol is deliberately small. A request is one JSON-encoded
// [event.Envelope] followed by a newline. A reply is one JSON-encoded
// [Response] followed by a newline. Requests are answered in order on a
// connection, so a connection is a strictly sequential request/response
// channel — which is what a single-threaded QCAlgorithm needs.
//
// The domain never imports this package: it lives outside internal/ precisely
// so that the `domain-purity` depguard rule keeps networking out of the
// reducer. See docs/architecture.md and docs/adr/0014-lean-go-transport.md.
package transport

import "github.com/richard-whittemore/TrendInvesting/internal/event"

// Protocol error codes. They are part of the wire contract: the adapter
// branches on them to decide between "retry", "drop this bar", and "enter safe
// mode", so they must stay stable.
const (
	// CodeInvalidFrame reports a frame that is not a JSON object.
	CodeInvalidFrame = "invalid_frame"
	// CodeInvalidEnvelope reports a frame that decodes but fails
	// [event.Envelope.Validate].
	CodeInvalidEnvelope = "invalid_envelope"
	// CodeOversized reports a frame longer than the negotiated maximum. The
	// server discards the remainder of the line and stays connected.
	CodeOversized = "oversized"
	// CodeDeciderFailed reports that the decision engine returned an error.
	CodeDeciderFailed = "decider_failed"
	// CodeTimeout reports that the decision engine did not answer within the
	// server's decision timeout.
	CodeTimeout = "timeout"
	// CodeUnavailable reports that the engine is shutting down and abandoned
	// the decision. It exists so that an orderly shutdown says so, instead of
	// reaching the adapter as an anonymous dropped connection.
	CodeUnavailable = "unavailable"
)

// Response is the server's reply to exactly one request frame. Exactly one of
// Envelope and Error is set.
type Response struct {
	Envelope *event.Envelope `json:"envelope,omitempty"`
	Error    *ProtocolError  `json:"error,omitempty"`
}

// ProtocolError is a failure the server can describe. It is deliberately
// distinct from a transport failure: a ProtocolError means the connection is
// still usable and the peer is still alive, which is the difference between
// "reject this bar" and "the decision engine is gone".
type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// CausationID echoes the request envelope's id whenever the server could
	// read it, so the adapter can tell which bar was rejected.
	CausationID string `json:"causation_id,omitempty"`
}

func (e *ProtocolError) Error() string {
	return "transport: " + e.Code + ": " + e.Message
}

func errorResponse(code, message, causationID string) Response {
	return Response{Error: &ProtocolError{
		Code:        code,
		Message:     message,
		CausationID: causationID,
	}}
}

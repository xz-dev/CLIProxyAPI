package bodyspool

import (
	"encoding/json"
	"fmt"
)

// Error is an explicit hop error (D9). Error() is the JSON body in OpenAI shape, so the existing
// handler error paths relay it unchanged; StatusCode() feeds clienterror.HTTPStatusFromError.
type Error struct {
	Status  int
	Code    string
	Message string
	Hop     string
	Limit   int64 // the enforced limit in bytes, when one applies
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Hop     string `json:"hop"`
	Limit   int64  `json:"limit,omitempty"`
}

func (e *Error) Error() string {
	typ := "server_error"
	if e.Status == 413 {
		typ = "invalid_request_error"
	}
	b, _ := json.Marshal(errorBody{Error: errorDetail{Message: e.Message, Type: typ, Code: e.Code, Hop: e.Hop, Limit: e.Limit}})
	return string(b)
}

// StatusCode returns the HTTP status of the error.
func (e *Error) StatusCode() int { return e.Status }

// IsRequestScoped marks every hop refusal as a fault of this request, not of a credential: the
// conductor stops without retry, rotation or cooldown (cliproxyexecutor.RequestScopedError).
func (e *Error) IsRequestScoped() bool { return true }

// FallbackTooLarge is the named 413 for a body above a fallback path's limit L.
func FallbackTooLarge(hop, path string, size, limit int64) *Error {
	return &Error{Status: 413, Code: "request_too_large", Hop: hop, Limit: limit,
		Message: fmt.Sprintf("%s: %d-byte request exceeds the %d-byte fallback limit of %s (not converted to the body spool)", hop, size, limit, path)}
}

// CeilingTooLarge is the named 413 for a body above a provider route's ceiling (D1).
func CeilingTooLarge(hop, route string, size, limit int64) *Error {
	return &Error{Status: 413, Code: "request_too_large", Hop: hop, Limit: limit,
		Message: fmt.Sprintf("%s: %d-byte request exceeds the %d-byte provider ceiling of %s", hop, size, limit, route)}
}

// TicketMissing refuses a body of TicketThreshold bytes or more that carries no ticket (D4).
func TicketMissing(hop string, size int64) *Error {
	return &Error{Status: 503, Code: "capacity_ticket_missing", Hop: hop, Limit: TicketThreshold,
		Message: fmt.Sprintf("%s received %d bytes (>= %d) without a capacity ticket", hop, size, int64(TicketThreshold))}
}

// TicketInvalid refuses a ticket that cannot be leased (D4).
func TicketInvalid(hop, why string) *Error {
	return &Error{Status: 503, Code: "capacity_ticket_invalid", Hop: hop, Message: fmt.Sprintf("%s capacity ticket invalid: %s", hop, why)}
}

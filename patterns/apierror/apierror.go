// Package apierror defines the single error envelope every handler, proxy and
// middleware in the gateway writes. The shape follows the Azure REST API
// guidelines (error.code / message / target / details / innererror) with two
// additions the gateway needs for clients: retryable and retryAfter.
//
// Why one envelope: clients, SDKs and dashboards branch on error.code, never on
// free-text messages. The HTTP status is transport detail; the code is the
// contract. See docs/07-services/errors.md.
package apierror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
)

// Reserved codes. Providers/services add their own codes; they must not reuse
// these. Keep this list in sync with docs/07-services/errors.md.
const (
	CodeMalformedRequest    = "MalformedRequest"
	CodeInvalidInput        = "InvalidInput"
	CodeUnauthenticated     = "Unauthenticated"
	CodePolicyDenied        = "PolicyDenied"
	CodeNotFound            = "NotFound"
	CodeConflict            = "Conflict"
	CodeIdempotencyReused   = "IdempotencyKeyReused"
	CodeRateLimited         = "RateLimited"
	CodeOverloaded          = "Overloaded"
	CodePayloadTooLarge     = "PayloadTooLarge"
	CodeDeadlineExceeded    = "DeadlineExceeded"
	CodeCanceled            = "Canceled"
	CodeUpstreamUnavailable = "UpstreamUnavailable"
	CodeUpstreamError       = "UpstreamError"
	CodeShuttingDown        = "ShuttingDown"
	CodeInternal            = "InternalError"
)

// StatusClientClosedRequest is the de-facto status (nginx, Traefik, Caddy
// access logs) for a client that went away before the response was written.
// It is never actually sent on the wire because the connection is gone; it
// exists so that logs and metrics can tell cancellation from failure.
const StatusClientClosedRequest = 499

// Detail is one entry in Error.Details.
type Detail struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Target  string `json:"target,omitempty"`
}

// Inner is a nested, more specific code. Clients should not depend on it.
type Inner struct {
	Code  string `json:"code"`
	Inner *Inner `json:"innererror,omitempty"`
}

// Error is the envelope. Status is not serialised; it is carried for the
// writer. Message must be safe to show to a caller: never include stack
// traces, SQL, hostnames or secrets.
type Error struct {
	Status     int      `json:"-"`
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Target     string   `json:"target,omitempty"`
	Retryable  bool     `json:"retryable"`
	RetryAfter int      `json:"retryAfter,omitempty"` // seconds; 0 means absent
	Hop        string   `json:"hop,omitempty"`
	Details    []Detail `json:"details,omitempty"`
	Inner      *Inner   `json:"innererror,omitempty"`

	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s (%d): %s: %v", e.Code, e.Status, e.Message, e.cause)
	}
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// Unwrap exposes the cause to errors.Is / errors.As. Causes are for logs, never
// for the wire.
func (e *Error) Unwrap() error { return e.cause }

// New creates an envelope with no cause.
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message, Retryable: defaultRetryable(status)}
}

// Wrap attaches a cause to a new envelope. The cause is logged, not sent.
func Wrap(cause error, status int, code, message string) *Error {
	e := New(status, code, message)
	e.cause = cause
	return e
}

// WithDetail appends a detail and returns the same error for chaining.
func (e *Error) WithDetail(code, target, message string) *Error {
	e.Details = append(e.Details, Detail{Code: code, Target: target, Message: message})
	return e
}

// WithRetryAfter marks the error retryable after the given number of seconds.
func (e *Error) WithRetryAfter(seconds int) *Error {
	e.Retryable = true
	e.RetryAfter = seconds
	return e
}

func defaultRetryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusBadGateway:
		return true
	}
	return false
}

// From converts any error into an envelope. Known errors map to precise codes;
// everything else becomes a 500 InternalError with a generic message so that
// internal details never leak. The original error is kept as the cause.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Wrap(err, http.StatusGatewayTimeout, CodeDeadlineExceeded, "the request did not complete before its deadline")
	case errors.Is(err, context.Canceled):
		return Wrap(err, StatusClientClosedRequest, CodeCanceled, "the request was canceled by the client")
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return Wrap(err, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			fmt.Sprintf("request body exceeds the limit of %d bytes", mbe.Limit))
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Wrap(err, http.StatusGatewayTimeout, CodeDeadlineExceeded, "upstream did not respond in time")
	}
	return Wrap(err, http.StatusInternalServerError, CodeInternal, "an internal error occurred")
}

// Write serialises the envelope. It sets the x-error-code header so proxies and
// CLIs can act without parsing the body, and Retry-After when the error says
// so. A 499 is written as 499 so that the access log records it; the client is
// gone anyway.
func Write(w http.ResponseWriter, err error) {
	e := From(err)
	if e == nil {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Error-Code", e.Code)
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(struct {
		Error *Error `json:"error"`
	}{Error: e})
}

// HTTPStatusForRPCCode maps gRPC/Connect codes to HTTP status, matching the
// table connect-go uses on the wire so that a gateway in front of Connect or
// gRPC services returns the same status a Connect client would compute.
func HTTPStatusForRPCCode(code string) int {
	switch code {
	case "canceled":
		return StatusClientClosedRequest // connect uses 499 as well
	case "unknown", "internal", "data_loss":
		return http.StatusInternalServerError
	case "invalid_argument", "out_of_range":
		return http.StatusBadRequest
	case "deadline_exceeded":
		return http.StatusGatewayTimeout
	case "not_found":
		return http.StatusNotFound
	case "already_exists", "aborted":
		return http.StatusConflict
	case "permission_denied":
		return http.StatusForbidden
	case "resource_exhausted":
		return http.StatusTooManyRequests
	case "failed_precondition":
		return http.StatusBadRequest
	case "unimplemented":
		return http.StatusNotImplemented
	case "unavailable":
		return http.StatusServiceUnavailable
	case "unauthenticated":
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

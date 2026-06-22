// Package korai is the Korai Platform SDK for Go.
//
// It exposes typed clients and helpers for building Go services on top of
// Korai Cloud — the orchestrator's HTTP+SSE API. The SDK is organised
// around seven module namespaces hung off a single Client value:
//
//   - korai.Auth      — login, signup, current user, JWT helpers
//   - korai.Tenant    — multi-tenant (organization) helpers
//   - korai.LLM       — chat completions + streaming + model listing
//   - korai.RAG       — vector retrieval + reranker primitives (stubs)
//   - korai.Audit     — append-only audit log (in-memory store today)
//   - korai.Tools     — Go-native tool registry + invocation
//   - korai.Billing   — credit balance, transactions, Stripe checkout
//
// Every method takes a context.Context and returns either a typed value or
// an error. Network failures surface as either *APIError (HTTP 4xx/5xx
// from Korai Cloud) or a plain error wrapping the underlying transport
// failure. The sentinel errors ErrUnauthorized, ErrNotFound and
// ErrRateLimited are matched via errors.Is on the returned error so
// callers don't have to type-assert *APIError to handle the common cases.
//
// Quick start:
//
//	cli := korai.New(korai.WithAPIKey("kfid_..."))
//	resp, err := cli.ChatComplete(ctx, korai.ChatRequest{
//	    Model:    "claude-opus-4-7",
//	    Messages: []korai.Message{{Role: "user", Content: "Hello"}},
//	})
package korai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors. All of them satisfy errors.Is when the underlying
// failure is a matching *APIError. Useful for switch-style handling
// without unwrapping:
//
//	if errors.Is(err, korai.ErrUnauthorized) {
//	    // re-login
//	}
var (
	// ErrUnauthorized is returned (wrapped) when Korai Cloud answers
	// with 401 or 403.
	ErrUnauthorized = errors.New("korai: unauthorized")
	// ErrNotFound is returned (wrapped) when Korai Cloud answers 404.
	ErrNotFound = errors.New("korai: not found")
	// ErrRateLimited is returned (wrapped) when Korai Cloud answers 429.
	// Inspect (*APIError).RetryAfter for the suggested back-off.
	ErrRateLimited = errors.New("korai: rate limited")
	// ErrNotImplemented marks an SDK call whose implementation is still
	// pending exposure on the Korai Cloud side.
	ErrNotImplemented = errors.New("korai: not implemented")
	// ErrInvalidConfig is returned by constructors when a required
	// option is missing or malformed.
	ErrInvalidConfig = errors.New("korai: invalid config")
	// ErrConnection wraps a transport-level failure that prevented the
	// request from reaching Korai Cloud (DNS, dial, TLS, reset). It is
	// distinct from an *APIError (which implies the server answered) and
	// from context.Canceled / context.DeadlineExceeded (caller-driven
	// cancellation, deliberately left unwrapped). Match it with
	// IsConnectionError.
	ErrConnection = errors.New("korai: connection error")
)

// APIError carries the structured payload returned by Korai Cloud on a
// non-2xx HTTP response. The orchestrator emits a body shaped like
// {"error": {"message": "...", "type": "..."}} and APIError flattens it
// into accessible fields.
type APIError struct {
	// StatusCode is the HTTP status code returned by Korai Cloud.
	StatusCode int
	// Code is the orchestrator-side error_type discriminator
	// (invalid_request, authentication_error, server_error, …).
	// May be empty when the orchestrator returned an unstructured body.
	Code string
	// Message is a human-readable description, extracted from the
	// response body when possible. Falls back to the status text.
	Message string
	// Details is the raw decoded JSON payload, useful for callers that
	// need fields not surfaced explicitly.
	Details map[string]any
	// RetryAfter, when non-zero, is the parsed Retry-After header in
	// seconds. Set on 429 responses.
	RetryAfter int
	// RequestID is the server-assigned correlation id, populated from the
	// X-Request-Id / Request-Id / X-Korai-Request-Id response header (in
	// that order). Empty when none was present. Quote it in bug reports.
	RequestID string
}

// Error formats the error with HTTP status, code and message.
func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("korai: HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("korai: HTTP %d: %s", e.StatusCode, e.Message)
}

// Is matches sentinel errors based on the HTTP status code so that
// errors.Is(err, korai.ErrUnauthorized) does the right thing without
// callers having to type-assert.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.StatusCode == 401 || e.StatusCode == 403
	case ErrNotFound:
		return e.StatusCode == 404
	case ErrRateLimited:
		return e.StatusCode == 429
	}
	return false
}

// statusEquals reports whether err is (or wraps) an *APIError whose
// StatusCode equals want.
func statusEquals(err error, want int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == want
}

// IsBadRequest reports whether err is an *APIError with HTTP status 400.
func IsBadRequest(err error) bool { return statusEquals(err, http.StatusBadRequest) }

// IsUnauthorized reports whether err is an *APIError with HTTP status 401.
// For the broader 401-or-403 notion, use errors.Is(err, ErrUnauthorized).
func IsUnauthorized(err error) bool { return statusEquals(err, http.StatusUnauthorized) }

// IsPermissionDenied reports whether err is an *APIError with HTTP status 403.
func IsPermissionDenied(err error) bool { return statusEquals(err, http.StatusForbidden) }

// IsNotFoundStatus reports whether err is an *APIError with HTTP status 404.
// It is the status-based companion to the ErrNotFound sentinel (named to
// avoid colliding with the existing errors.Is(err, ErrNotFound) idiom).
func IsNotFoundStatus(err error) bool { return statusEquals(err, http.StatusNotFound) }

// IsUnprocessable reports whether err is an *APIError with HTTP status 422.
func IsUnprocessable(err error) bool { return statusEquals(err, http.StatusUnprocessableEntity) }

// IsRateLimited reports whether err is an *APIError with HTTP status 429.
// Inspect (*APIError).RetryAfter for the suggested back-off.
func IsRateLimited(err error) bool { return statusEquals(err, http.StatusTooManyRequests) }

// IsServerError reports whether err is an *APIError with an HTTP status of
// 500 or greater.
func IsServerError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode >= 500
}

// IsConnectionError reports whether err denotes a transport-level failure
// (wrapping ErrConnection) that kept the request from reaching Korai Cloud.
// Caller-driven cancellation (context.Canceled / context.DeadlineExceeded)
// is deliberately not treated as a connection error.
func IsConnectionError(err error) bool { return errors.Is(err, ErrConnection) }

// requestIDFromHeader extracts the server correlation id from the first
// matching header among X-Request-Id, Request-Id and X-Korai-Request-Id.
func requestIDFromHeader(h http.Header) string {
	for _, k := range []string{"X-Request-Id", "Request-Id", "X-Korai-Request-Id"} {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// wrapTransportError classifies a non-HTTP error returned by the transport.
// Caller-driven cancellation (context.Canceled / context.DeadlineExceeded)
// is wrapped verbatim so callers can still match it with errors.Is; every
// other transport failure is wrapped with ErrConnection so IsConnectionError
// detects it.
func wrapTransportError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("korai: HTTP request failed: %w", err)
	}
	return fmt.Errorf("korai: connection error: %w: %v", ErrConnection, err)
}

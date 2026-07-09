package korai

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAPIErrorString(t *testing.T) {
	e := &APIError{StatusCode: 401, Code: "authentication_error", Message: "bad credentials"}
	if !strings.Contains(e.Error(), "authentication_error") {
		t.Fatalf("error string missing code: %q", e.Error())
	}
	if !strings.Contains(e.Error(), "401") {
		t.Fatalf("error string missing status: %q", e.Error())
	}
}

func TestAPIErrorStringWithoutCode(t *testing.T) {
	e := &APIError{StatusCode: 500, Message: "boom"}
	if !strings.Contains(e.Error(), "500") {
		t.Fatalf("error string missing status: %q", e.Error())
	}
}

func TestSentinelErrorIdentity(t *testing.T) {
	if errors.Is(ErrUnauthorized, ErrNotFound) {
		t.Fatal("sentinel errors should be distinct")
	}
}

func TestAPIErrorIsRejectsUnknownTarget(t *testing.T) {
	e := &APIError{StatusCode: 500}
	if errors.Is(e, ErrUnauthorized) {
		t.Fatal("500 should not match ErrUnauthorized")
	}
}

// statusPredicate maps each status predicate to the single status it should
// fire on, so the table test can assert exclusivity.
func TestStatusPredicates(t *testing.T) {
	preds := map[string]struct {
		fn     func(error) bool
		status int
	}{
		"IsBadRequest":       {IsBadRequest, 400},
		"IsUnauthorized":     {IsUnauthorized, 401},
		"IsPermissionDenied": {IsPermissionDenied, 403},
		"IsNotFoundStatus":   {IsNotFoundStatus, 404},
		"IsUnprocessable":    {IsUnprocessable, 422},
		"IsRateLimited":      {IsRateLimited, 429},
	}
	for _, status := range []int{400, 401, 403, 404, 422, 429} {
		err := error(&APIError{StatusCode: status})
		for name, p := range preds {
			want := p.status == status
			if got := p.fn(err); got != want {
				t.Errorf("status %d: %s = %v, want %v", status, name, got, want)
			}
		}
		if IsServerError(err) {
			t.Errorf("status %d: IsServerError should be false", status)
		}
	}
}

func TestIsServerError(t *testing.T) {
	if !IsServerError(&APIError{StatusCode: 503}) {
		t.Fatal("503 should be a server error")
	}
	if !IsServerError(&APIError{StatusCode: 500}) {
		t.Fatal("500 should be a server error")
	}
	if IsServerError(&APIError{StatusCode: 404}) {
		t.Fatal("404 should not be a server error")
	}
	if IsServerError(errors.New("plain")) {
		t.Fatal("non-APIError should not be a server error")
	}
}

// TestPredicatesAgainstLiveServer exercises the predicates end-to-end
// through an httptest server so the error actually flows through
// parseAPIError + the transport.
func TestPredicatesAgainstLiveServer(t *testing.T) {
	cases := []struct {
		status int
		pred   func(error) bool
	}{
		{400, IsBadRequest},
		{403, IsPermissionDenied},
		{422, IsUnprocessable},
		{503, IsServerError},
	}
	for _, tc := range cases {
		status := tc.status
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":{"message":"x","type":"y"}}`))
		}))
		cli := New(WithBaseURL(srv.URL), WithMaxRetries(0)) // skip retry on 503
		err := cli.doRequest(context.Background(), "GET", "/x", nil, nil)
		if !tc.pred(err) {
			t.Errorf("status %d: predicate returned false for %v", status, err)
		}
		srv.Close()
	}
}

func TestRequestIDSurfacedOnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-12345")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad","type":"invalid_request"}}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	err := cli.doRequest(context.Background(), "GET", "/x", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not APIError: %v", err)
	}
	if apiErr.RequestID != "req-12345" {
		t.Fatalf("RequestID = %q, want req-12345", apiErr.RequestID)
	}
}

func TestRequestIDFromHeaderPrecedence(t *testing.T) {
	h := http.Header{}
	h.Set("Request-Id", "second")
	h.Set("X-Korai-Request-Id", "third")
	if got := requestIDFromHeader(h); got != "second" {
		t.Fatalf("expected Request-Id fallback, got %q", got)
	}
	h.Set("X-Request-Id", "first")
	if got := requestIDFromHeader(h); got != "first" {
		t.Fatalf("expected X-Request-Id precedence, got %q", got)
	}
	if got := requestIDFromHeader(http.Header{}); got != "" {
		t.Fatalf("expected empty when absent, got %q", got)
	}
}

func TestIsConnectionErrorOnDialFailure(t *testing.T) {
	// Bind a listener then close it so the port is reliably refused
	// (connection-refused, not a timeout — which would surface as a
	// context.DeadlineExceeded and is deliberately NOT a connection error).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cli := New(
		WithBaseURL("http://"+addr),
		WithMaxRetries(0),
		WithTimeout(2*time.Second),
	)
	err = cli.doRequest(context.Background(), "GET", "/x", nil, nil)
	if err == nil {
		t.Fatal("expected dial failure")
	}
	if !IsConnectionError(err) {
		t.Fatalf("expected IsConnectionError true, got %v", err)
	}
	// A dial failure must not be misclassified as an APIError.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatal("dial failure should not be an *APIError")
	}
}

func TestContextCancelNotConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := cli.doRequest(ctx, "GET", "/x", nil, nil)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if IsConnectionError(err) {
		t.Fatalf("context cancellation must not be a connection error: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is(context.Canceled), got %v", err)
	}
}

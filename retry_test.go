package korai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// retryAfterZero writes a transient status with Retry-After: 0 so the
// backoff is instant — keeps the retry tests fast while exercising the
// header-honoring path.
func retryAfterZero(w http.ResponseWriter, status int) {
	w.Header().Set("Retry-After", "0")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"message":"transient","type":"transient"}}`))
}

func TestRetryTransientThenSuccess(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			retryAfterZero(w, 503)
			return
		}
		_, _ = w.Write([]byte(`{"balance_eur": 1.5}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL)) // default maxRetries = 2
	bal, err := cli.GetBalance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bal != 1.5 {
		t.Fatalf("balance = %v", bal)
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestRetryExhausted(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		retryAfterZero(w, 503)
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(2))
	_, err := cli.GetBalance(context.Background())
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 {
		t.Fatalf("err = %v", err)
	}
	if got := atomic.LoadInt32(&n); got != 3 {
		t.Fatalf("attempts = %d, want 3 (1 + 2 retries)", got)
	}
}

func TestNoRetryOnNonRetryable(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"bad","type":"invalid_request"}}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	_, err := cli.GetBalance(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on 400)", got)
	}
}

func TestMaxRetriesZeroDisables(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		retryAfterZero(w, 503)
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	_, err := cli.GetBalance(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("attempts = %d, want 1 (retries disabled)", got)
	}
}

package korai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestWithOptionsReturnsDistinctClient verifies the clone is a separate
// pointer and preserves inherited config (baseURL, apiKey).
func TestWithOptionsReturnsDistinctClient(t *testing.T) {
	orig := New(WithBaseURL("https://example.invalid"), WithAPIKey("kfid_abc"))
	clone := orig.WithOptions(WithMaxRetries(0))

	if clone == orig {
		t.Fatal("WithOptions returned the same pointer")
	}
	if clone.BaseURL() != "https://example.invalid" {
		t.Fatalf("clone baseURL = %q", clone.BaseURL())
	}
	if clone.APIKey() != "kfid_abc" {
		t.Fatalf("clone apiKey = %q", clone.APIKey())
	}
	// The original must be untouched: it keeps its default maxRetries.
	if orig.maxRetries != 2 {
		t.Fatalf("orig maxRetries mutated to %d", orig.maxRetries)
	}
	if clone.maxRetries != 0 {
		t.Fatalf("clone maxRetries = %d, want 0", clone.maxRetries)
	}
	// Tools/Audit are shared by reference.
	if clone.Tools != orig.Tools {
		t.Fatal("Tools should be shared by reference")
	}
	if clone.Audit != orig.Audit {
		t.Fatal("Audit should be shared by reference")
	}
}

// TestWithOptionsMaxRetriesOverride asserts that a clone built with
// WithMaxRetries(0) does NOT retry a 503, while the original (default
// maxRetries=2) does.
func TestWithOptionsMaxRetriesOverride(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		retryAfterZero(w, 503)
	}))
	t.Cleanup(srv.Close)

	orig := New(WithBaseURL(srv.URL)) // default maxRetries = 2
	noRetry := orig.WithOptions(WithMaxRetries(0))

	// Clone: no retries -> exactly one attempt.
	atomic.StoreInt32(&n, 0)
	if _, err := noRetry.GetBalance(context.Background()); err == nil {
		t.Fatal("expected error from no-retry clone")
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("clone attempts = %d, want 1 (retries disabled)", got)
	}

	// Original: still retries -> 1 + 2 = 3 attempts.
	atomic.StoreInt32(&n, 0)
	if _, err := orig.GetBalance(context.Background()); err == nil {
		t.Fatal("expected error from original after exhausting retries")
	}
	if got := atomic.LoadInt32(&n); got != 3 {
		t.Fatalf("orig attempts = %d, want 3 (1 + 2 retries)", got)
	}
}

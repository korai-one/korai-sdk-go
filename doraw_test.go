package korai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoRawReturnsResponseWithHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom-Header", "korai-raw")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("not json at all"))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	resp, err := cli.DoRaw(context.Background(), http.MethodGet, "/anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Custom-Header"); got != "korai-raw" {
		t.Fatalf("header = %q, want korai-raw", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "not json at all" {
		t.Fatalf("body = %q", body)
	}
}

func TestDoRawDoesNotMapNon2xxToAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	t.Cleanup(srv.Close)

	// Disable retries so the 503 comes straight back rather than being
	// retried (and ultimately surfaced) by the transport.
	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	resp, err := cli.DoRaw(context.Background(), http.MethodGet, "/down", nil)
	if err != nil {
		t.Fatalf("expected nil error for 503, got %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

package korai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient is a small helper that wires a Client to an
// httptest.Server. The handler implements only the routes the test
// exercises; everything else 404s.
func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cli := New(WithBaseURL(srv.URL), WithAPIKey("test-key"))
	return cli, srv
}

func TestNewAppliesDefaults(t *testing.T) {
	cli := New()
	if cli.BaseURL() != DefaultBaseURL {
		t.Fatalf("expected default base URL, got %q", cli.BaseURL())
	}
	if cli.HTTPClient() == nil {
		t.Fatal("expected default http client")
	}
	if cli.Tools == nil {
		t.Fatal("expected default tool registry")
	}
	if cli.Audit == nil {
		t.Fatal("expected default audit module")
	}
}

func TestNewAppliesOptions(t *testing.T) {
	cli := New(
		WithAPIKey("kfid_xxx"),
		WithBaseURL("https://staging.korai.one/"),
		WithOrganizationID("org-1"),
		WithUserAgent("custom/1.0"),
		WithTimeout(5*time.Second),
	)
	if cli.APIKey() != "kfid_xxx" {
		t.Fatalf("apiKey = %q", cli.APIKey())
	}
	if cli.BaseURL() != "https://staging.korai.one" {
		t.Fatalf("baseURL = %q", cli.BaseURL())
	}
	if cli.OrganizationID() != "org-1" {
		t.Fatalf("orgID = %q", cli.OrganizationID())
	}
}

func TestDefaultHeadersIncludesAuth(t *testing.T) {
	cli := New(WithAPIKey("kfid_test"), WithOrganizationID("org-42"))
	h := cli.defaultHeaders()
	if got := h.Get("Authorization"); got != "Bearer kfid_test" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := h.Get("X-Korai-Organization"); got != "org-42" {
		t.Fatalf("X-Korai-Organization = %q", got)
	}
	if !strings.Contains(h.Get("User-Agent"), "korai-sdk-go") {
		t.Fatalf("UA = %q", h.Get("User-Agent"))
	}
}

func TestUseTokenSwapsCredential(t *testing.T) {
	cli := New()
	if cli.APIKey() != "" {
		t.Fatalf("expected empty key initially")
	}
	cli.UseToken("kfid_new")
	if cli.APIKey() != "kfid_new" {
		t.Fatalf("UseToken did not update")
	}
}

func TestDoRequestPropagatesAuthHeader(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithAPIKey("kfid_xyz"))
	var out map[string]any
	if err := cli.doRequest(context.Background(), "GET", "/anything", nil, &out); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if seen != "Bearer kfid_xyz" {
		t.Fatalf("Authorization seen by server = %q", seen)
	}
}

func TestDoRequestParsesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid email or password","type":"authentication_error"}}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	err := cli.doRequest(context.Background(), "POST", "/auth/login", map[string]string{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 401 {
		t.Fatalf("status = %d", apiErr.StatusCode)
	}
	if apiErr.Code != "authentication_error" {
		t.Fatalf("code = %q", apiErr.Code)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expected errors.Is(ErrUnauthorized)")
	}
}

func TestAPIErrorIsMatchesNotFound(t *testing.T) {
	apiErr := &APIError{StatusCode: 404, Message: "missing"}
	if !errors.Is(apiErr, ErrNotFound) {
		t.Fatal("expected ErrNotFound match")
	}
	if errors.Is(apiErr, ErrUnauthorized) {
		t.Fatal("did not expect ErrUnauthorized match")
	}
}

func TestAPIErrorIsMatchesRateLimit(t *testing.T) {
	apiErr := &APIError{StatusCode: 429, RetryAfter: 30}
	if !errors.Is(apiErr, ErrRateLimited) {
		t.Fatal("expected ErrRateLimited")
	}
}

func TestParseAPIErrorRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"too many","type":"rate_limited"}}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0)) // test error mapping, not retry
	err := cli.doRequest(context.Background(), "GET", "/x", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not APIError: %v", err)
	}
	if apiErr.RetryAfter != 12 {
		t.Fatalf("retry_after = %d", apiErr.RetryAfter)
	}
}

func TestParseAPIErrorFallbackOnNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`internal explosion`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0)) // test error mapping, not retry
	err := cli.doRequest(context.Background(), "GET", "/x", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not APIError: %v", err)
	}
	if !strings.Contains(apiErr.Message, "internal explosion") {
		t.Fatalf("expected raw body in message, got %q", apiErr.Message)
	}
}

func TestDoRequestSerialisesJSONBody(t *testing.T) {
	var receivedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedBody)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	payload := map[string]string{"hello": "world"}
	if err := cli.doRequest(context.Background(), "POST", "/x", payload, nil); err != nil {
		t.Fatal(err)
	}
	if receivedBody["hello"] != "world" {
		t.Fatalf("body not transmitted: %#v", receivedBody)
	}
}

func TestDoRequestRespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the connection open briefly; the test cancels first.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := cli.doRequest(ctx, "GET", "/", nil, nil)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestNewSeedsAPIKeyFromEnv(t *testing.T) {
	t.Setenv("KORAI_API_KEY", "env-key")
	cli := New()
	if cli.APIKey() != "env-key" {
		t.Fatalf("expected env api key, got %q", cli.APIKey())
	}
}

func TestWithAPIKeyOverridesEnv(t *testing.T) {
	t.Setenv("KORAI_API_KEY", "env-key")
	cli := New(WithAPIKey("explicit-key"))
	if cli.APIKey() != "explicit-key" {
		t.Fatalf("explicit option should win, got %q", cli.APIKey())
	}
}

func TestNewSeedsBaseURLFromEnv(t *testing.T) {
	t.Setenv("KORAI_BASE_URL", "https://env.korai.one/")
	cli := New()
	if cli.BaseURL() != "https://env.korai.one" {
		t.Fatalf("expected env base url (trimmed), got %q", cli.BaseURL())
	}
}

func TestWithBaseURLOverridesEnv(t *testing.T) {
	t.Setenv("KORAI_BASE_URL", "https://env.korai.one")
	cli := New(WithBaseURL("https://explicit.korai.one"))
	if cli.BaseURL() != "https://explicit.korai.one" {
		t.Fatalf("explicit option should win, got %q", cli.BaseURL())
	}
}

func TestNewEmptyEnvKeepsDefaultBaseURL(t *testing.T) {
	t.Setenv("KORAI_BASE_URL", "")
	cli := New()
	if cli.BaseURL() != DefaultBaseURL {
		t.Fatalf("empty env should keep default, got %q", cli.BaseURL())
	}
}

func TestIsNotImplementedHelper(t *testing.T) {
	if !IsNotImplemented(ErrNotImplemented) {
		t.Fatal("expected true")
	}
	if IsNotImplemented(errors.New("other")) {
		t.Fatal("expected false")
	}
}

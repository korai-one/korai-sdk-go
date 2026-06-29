package korai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/korai-one/korai-sdk-go/koraiapi"
)

// Version is the SDK version stamped into the User-Agent header.
const Version = "0.1.0"

const (
	// DefaultBaseURL is Korai Cloud's production endpoint. Override
	// with WithBaseURL for staging / self-hosted deployments. Aligned
	// with @korai/sdk-py and @korai/sdk so all three SDKs default to
	// the same public domain.
	DefaultBaseURL = "https://cloud.korai.one"
	// DefaultTimeout is applied to non-streaming HTTP requests when no
	// custom http.Client is supplied. Streaming requests honour the
	// caller's context.Context instead so long-running SSE streams
	// don't get cut off.
	DefaultTimeout = 60 * time.Second
)

// Client is the top-level entry point for the Korai SDK. Every module
// (Auth, Tenant, LLM, RAG, Audit, Tools, Billing) lives off this type.
//
// Client values are safe for concurrent use by multiple goroutines once
// constructed. The auth token can be rotated via UseToken at any time;
// in-flight requests pick up the new value on their next read of it.
type Client struct {
	apiKey         string
	baseURL        string
	organizationID string
	userAgent      string
	httpClient     *http.Client
	timeout        time.Duration
	maxRetries     int

	// suppliedHTTPClient is true when the caller injected the
	// *http.Client via WithHTTPClient. The SDK then treats the transport
	// as caller-owned: it is never wrapped with retry/backoff, and
	// WithOptions reuses it rather than rebuilding a default one.
	suppliedHTTPClient bool

	// gen is the generated transport core (koraiapi), built from the
	// single source of truth specs/openapi.yaml. Module methods route
	// their HTTP through it so endpoint paths come from the spec; a
	// request editor injects auth headers and genDecode maps non-2xx to
	// *APIError. We use the raw *Client (not ClientWithResponses) and
	// decode bodies into the SDK's own types — the generated typed
	// response parsers reject non-UUID test ids on `format: uuid` fields.
	// See docs/CODEGEN.md.
	gen *koraiapi.Client

	// Tools is a Go-native tool registry. Unlike the Python/JS SDKs
	// where the registry is wired through the client constructor, the
	// Go registry is exposed as a standalone field so callers can
	// share it across multiple clients or use it without a client at
	// all.
	Tools *ToolRegistry

	// Audit is an in-process audit log. Callers can swap the
	// underlying store via WithAuditStore. By default it uses an
	// in-memory implementation with chained SHA-256 hashes.
	Audit *AuditModule
}

// ClientOption configures a Client at construction time.
type ClientOption func(*Client)

// WithAPIKey sets the Bearer token attached to every request as
// "Authorization: Bearer <key>". Accepts both Korai API keys
// (kfid_...) and raw JWTs.
func WithAPIKey(k string) ClientOption {
	return func(c *Client) { c.apiKey = k }
}

// WithBaseURL overrides the orchestrator base URL. Trailing slash is
// stripped. Useful for staging environments and tests pointing at
// httptest.Server.
func WithBaseURL(u string) ClientOption {
	return func(c *Client) {
		c.baseURL = strings.TrimRight(u, "/")
	}
}

// WithLocalWorker points the client at a Korai worker running locally,
// bypassing the orchestrator and the network. A worker started in local
// mode exposes the same OpenAI-compatible surface (/v1/chat/completions,
// /v1/models, /health) on a loopback port, so every module keeps working
// — only the base URL changes. Local workers require no credentials, so
// this also clears any API key. Discover a running worker's URL with
// DiscoverLocalWorker, or pass one explicitly:
//
//	if info, ok := korai.DiscoverLocalWorker(ctx); ok {
//	    c := korai.New(korai.WithLocalWorker(info.URL))
//	}
func WithLocalWorker(url string) ClientOption {
	return func(c *Client) {
		c.baseURL = strings.TrimRight(url, "/")
		c.apiKey = ""
	}
}

// WithOrganizationID scopes every request to a specific tenant. The
// header X-Korai-Organization is set; the orchestrator dispatches it
// through the auth middleware.
func WithOrganizationID(id string) ClientOption {
	return func(c *Client) { c.organizationID = id }
}

// WithHTTPClient injects a custom *http.Client. Use this to plug in
// retries, custom transports (mTLS), or a shared connection pool.
// When supplied, WithTimeout is ignored — set the timeout on your
// own http.Client instead.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = h
		c.suppliedHTTPClient = true
	}
}

// WithTimeout sets the per-request timeout for the default
// http.Client. Has no effect when WithHTTPClient is supplied.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.timeout = d }
}

// WithMaxRetries sets the maximum automatic retries for transient
// failures (HTTP 408/409/429/5xx and connection errors), with
// exponential backoff + jitter, honoring Retry-After. Default 2; 0
// disables. Only the default http.Client is wrapped — when you supply
// your own via WithHTTPClient, add retry transport yourself.
func WithMaxRetries(n int) ClientOption {
	return func(c *Client) { c.maxRetries = n }
}

// WithUserAgent overrides the default User-Agent header.
func WithUserAgent(ua string) ClientOption {
	return func(c *Client) { c.userAgent = ua }
}

// WithAuditStore swaps the in-memory audit store for a custom
// implementation (Postgres, SQLite, …).
func WithAuditStore(s AuditStore) ClientOption {
	return func(c *Client) {
		c.Audit = NewAuditModule(s)
	}
}

// New builds a Client with the given options. Sensible defaults are
// applied for every field not touched by an option.
//
// Example:
//
//	cli := korai.New(
//	    korai.WithAPIKey(os.Getenv("KORAI_API_KEY")),
//	    korai.WithBaseURL("https://staging.korai.one"),
//	    korai.WithTimeout(30 * time.Second),
//	)
func New(opts ...ClientOption) *Client {
	c := &Client{
		baseURL:    DefaultBaseURL,
		timeout:    DefaultTimeout,
		userAgent:  "korai-sdk-go/" + Version,
		maxRetries: 2,
		Tools:      NewToolRegistry(),
	}
	// Seed from the environment BEFORE applying options so an explicit
	// WithAPIKey / WithBaseURL always wins over KORAI_API_KEY /
	// KORAI_BASE_URL, which in turn win over the hardcoded defaults. An
	// empty env var is ignored so DefaultBaseURL is preserved.
	if k := os.Getenv("KORAI_API_KEY"); k != "" {
		c.apiKey = k
	}
	if u := os.Getenv("KORAI_BASE_URL"); u != "" {
		c.baseURL = strings.TrimRight(u, "/")
	}
	for _, o := range opts {
		o(c)
	}
	c.finalize(!c.suppliedHTTPClient)
	if c.Audit == nil {
		c.Audit = NewAuditModule(NewInMemoryAuditStore())
	}
	return c
}

// finalize builds the http.Client (when none was supplied), wraps its
// transport with retry/backoff, and (re)builds the generated transport
// core from the current field set. It is the single construction path
// shared by New and WithOptions so a cloned client's transport + gen
// reflect any overridden maxRetries/timeout/token.
//
// ownsHTTPClient reports whether the *http.Client is SDK-created (and
// therefore safe to mutate). A caller-supplied client (WithHTTPClient)
// is never wrapped — they own its transport and timeout.
func (c *Client) finalize(ownsHTTPClient bool) {
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: c.timeout}
		ownsHTTPClient = true
	}
	// Wrap the default client's transport with retry/backoff. We don't
	// mutate a caller-supplied http.Client (WithHTTPClient) — they own it.
	if ownsHTTPClient && c.maxRetries > 0 {
		c.httpClient.Transport = newRetryTransport(http.DefaultTransport, c.maxRetries)
	}
	// Build the generated transport core. The request editor copies the
	// dynamic header set (auth token can be rotated via UseToken) onto
	// every request. NewClientWithResponses only errors on an empty
	// server URL, which can't happen here (baseURL defaults are set).
	gen, err := koraiapi.NewClient(
		c.baseURL,
		koraiapi.WithHTTPClient(c.httpClient),
		koraiapi.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			for k, v := range c.defaultHeaders() {
				req.Header[k] = v
			}
			return nil
		}),
	)
	if err == nil {
		c.gen = gen
	}
}

// WithOptions returns a NEW *Client that is a shallow copy of the
// receiver with the given options applied on top. The clone gets a
// freshly built http transport + generated core so overridden
// maxRetries / timeout / token take effect; Tools and Audit are shared
// by reference with the receiver. The receiver is left untouched.
//
// Per-call timeout and cancellation are already handled by the
// context.Context passed to each method, so WithOptions is mainly for
// per-request maxRetries / timeout / token overrides:
//
//	noRetry := cli.WithOptions(korai.WithMaxRetries(0))
//	resp, err := noRetry.ChatComplete(ctx, req)
func (c *Client) WithOptions(opts ...ClientOption) *Client {
	clone := *c
	// Drop the inherited (SDK-owned) http.Client so finalize rebuilds a
	// fresh one reflecting any overridden maxRetries/timeout. A clone
	// reusing the parent's wrapped transport would silently ignore a
	// WithMaxRetries override. A caller-supplied client is kept unless an
	// option replaces it.
	if !clone.suppliedHTTPClient {
		clone.httpClient = nil
	}
	for _, o := range opts {
		o(&clone)
	}
	clone.finalize(!clone.suppliedHTTPClient)
	return &clone
}

// genDecode consumes a (*http.Response, error) pair returned by a
// generated koraiapi client method: it surfaces transport errors,
// reads + closes the body, maps non-2xx to *APIError (via parseAPIError),
// and JSON-decodes a 2xx body into out (when non-nil). This is the
// single bridge between the generated transport and the SDK's error +
// type model — the analogue of the TS response interceptor.
func (c *Client) genDecode(httpResp *http.Response, callErr error, out any) error {
	if callErr != nil {
		return wrapTransportError(callErr)
	}
	if httpResp == nil {
		return fmt.Errorf("korai: nil HTTP response from generated client")
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("korai: read response body: %w", err)
	}
	if httpResp.StatusCode >= 400 {
		return parseAPIError(httpResp, body)
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("korai: decode response: %w", err)
	}
	return nil
}

// BaseURL returns the orchestrator URL the client was configured with.
func (c *Client) BaseURL() string { return c.baseURL }

// APIKey returns the Bearer token currently attached to requests.
// Returns an empty string when none was configured.
func (c *Client) APIKey() string { return c.apiKey }

// OrganizationID returns the X-Korai-Organization scope, if any.
func (c *Client) OrganizationID() string { return c.organizationID }

// UseToken rotates the Bearer token at runtime — useful after a
// successful Login() or token refresh.
func (c *Client) UseToken(token string) { c.apiKey = token }

// SetOrganizationID switches the tenant scope for subsequent
// requests.
func (c *Client) SetOrganizationID(id string) { c.organizationID = id }

// HTTPClient returns the underlying *http.Client. Exposed for tests
// that want to swap transport.
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// defaultHeaders builds the per-request header set.
func (c *Client) defaultHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", c.userAgent)
	h.Set("Accept", "application/json")
	if c.apiKey != "" {
		h.Set("Authorization", "Bearer "+c.apiKey)
	}
	if c.organizationID != "" {
		h.Set("X-Korai-Organization", c.organizationID)
	}
	return h
}

// doRequest is the workhorse used by every module. It marshals
// `body` (when non-nil), attaches headers, sends the request, decodes
// 2xx JSON into `out`, and translates 4xx/5xx into *APIError.
//
// `out` may be nil if the caller doesn't care about the response
// body, e.g. on a 204 No Content.
func (c *Client) doRequest(ctx context.Context, method, path string, body, out any) error {
	req, err := c.buildRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return wrapTransportError(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("korai: read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return parseAPIError(resp, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("korai: decode response: %w", err)
	}
	return nil
}

// doRawRequest performs an HTTP request and returns the raw response
// for callers that need to handle SSE / NDJSON streams directly.
// The returned response body MUST be closed by the caller.
func (c *Client) doRawRequest(ctx context.Context, method, path string, body any) (*http.Response, error) {
	req, err := c.buildRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, wrapTransportError(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, parseAPIError(resp, raw)
	}
	return resp, nil
}

// DoRaw is the low-level escape hatch: it builds the request (auth
// headers, JSON body) and sends it through the client's transport
// (retry/backoff still applies on the default client), returning the
// raw *http.Response WITHOUT mapping non-2xx status codes to *APIError.
//
// Unlike doRequest / doRawRequest, the caller is handed the response
// for any status >= 100: inspect resp.StatusCode, read headers, or
// decode a non-JSON body yourself. A non-nil error is returned ONLY for
// transport-level failures (connection reset, DNS, context cancelled);
// an HTTP 4xx/5xx comes back as a normal *http.Response with nil error.
//
// The caller MUST close resp.Body. Use this when you need raw access to
// headers, status, or non-JSON payloads that the typed module methods
// would otherwise swallow.
func (c *Client) DoRaw(ctx context.Context, method, path string, body any) (*http.Response, error) {
	req, err := c.buildRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, wrapTransportError(err)
	}
	return resp, nil
}

// buildRequest assembles the *http.Request, marshalling JSON body if
// any. Used by both doRequest and doRawRequest.
func (c *Client) buildRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("korai: marshal request body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("korai: build request: %w", err)
	}
	for k, v := range c.defaultHeaders() {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// parseAPIError extracts a structured *APIError from the orchestrator
// envelope `{"error": {"message": "...", "type": "..."}}`. It falls
// back to a generic error if the body isn't JSON-shaped.
func parseAPIError(resp *http.Response, raw []byte) error {
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		Message:    http.StatusText(resp.StatusCode),
		RequestID:  requestIDFromHeader(resp.Header),
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil {
			apiErr.RetryAfter = n
		}
	}
	if len(raw) == 0 {
		return apiErr
	}
	var envelope struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error != nil {
		apiErr.Details = envelope.Error
		if msg, ok := envelope.Error["message"].(string); ok && msg != "" {
			apiErr.Message = msg
		}
		if t, ok := envelope.Error["type"].(string); ok {
			apiErr.Code = t
		}
		return apiErr
	}
	// Fallback: stash the raw body in details and use it as message
	// for a top-level "error" string field.
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err == nil {
		apiErr.Details = generic
		if msg, ok := generic["error"].(string); ok && msg != "" {
			apiErr.Message = msg
		}
	} else {
		// Not JSON — keep the raw text as the message.
		apiErr.Message = strings.TrimSpace(string(raw))
		if apiErr.Message == "" {
			apiErr.Message = http.StatusText(resp.StatusCode)
		}
	}
	return apiErr
}

// IsNotImplemented reports whether err denotes a method whose
// orchestrator-side implementation is still pending. Convenience
// wrapper around errors.Is.
func IsNotImplemented(err error) bool {
	return errors.Is(err, ErrNotImplemented)
}

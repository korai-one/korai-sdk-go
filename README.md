# @korai/sdk-go

Korai Platform SDK for Go — typed clients for Korai Cloud's HTTP+SSE
API. Use this from Go services that talk to the orchestrator: server
microservices, the future Korai Kode v2, or any backend that needs
chat completions, billing or audit logging without pulling in a
Python or Node.js runtime.

This package is part of the Couche 3 of the Korai architecture (see
`docs/TARGET_ARCHITECTURE.md`).

## Status

`v0.1.0` — scaffolded with end-to-end coverage of the endpoints the
orchestrator exposes today. RAG and tenant modules return
`ErrNotImplemented` until the matching cloud routes ship.

| Module    | Status        | Notes                                          |
|-----------|---------------|------------------------------------------------|
| `client`  | Implemented   | `New`, options, JSON helpers, error envelope   |
| `auth`    | Implemented   | `Login`, `Register`, `Me`, `Logout`, `ParseJWT`|
| `llm`     | Implemented   | `ChatComplete`, `ChatStream`, `ListModels`     |
| `tools`   | Implemented   | Registry, schemas, `FuncTool` helper           |
| `audit`   | Implemented   | In-memory store with chained SHA-256 hashes    |
| `billing` | Implemented   | `GetBalance`, `ListPackages`, `CreateCheckout` |
| `tenant`  | Stub          | Pending tenant API on Korai Cloud              |
| `rag`     | Stub          | Pending public retrieval endpoints             |

Anything in "Stub" returns `korai.ErrNotImplemented`. Match it via
`errors.Is(err, korai.ErrNotImplemented)` or `korai.IsNotImplemented(err)`.

## Install

```bash
go get github.com/korai-one/korai-sdk-go@latest
```

> Source of truth lives in the [`korai-one/korai`](https://github.com/korai-one/korai)
> monorepo under `korai-platform/packages/sdk-go`; this repo is a read-only
> mirror published on each release. File issues / PRs against the monorepo.

The module has **zero external dependencies** — only stdlib. This
keeps the dependency graph clean for downstream Korai services and
avoids transitive vulnerabilities in CI.

Go 1.26+ required.

## Quickstart

```go
package main

import (
    "context"
    "fmt"
    "io"
    "os"

    korai "github.com/korai-one/korai-sdk-go"
)

func main() {
    cli := korai.New(
        korai.WithAPIKey(os.Getenv("KORAI_API_KEY")),
        // The default already targets https://cloud.korai.one — only set
        // WithBaseURL when targeting staging or a self-hosted instance.
    )

    ctx := context.Background()
    resp, err := cli.ChatComplete(ctx, korai.ChatRequest{
        Model:     "auto",
        MaxTokens: 256,
        Messages: []korai.Message{
            {Role: "user", Content: "Bonjour, Korai !"},
        },
    })
    if err != nil {
        panic(err)
    }
    fmt.Println(resp.Choices[0].Message.Content)

    // Streaming variant.
    events, err := cli.ChatStream(ctx, korai.ChatRequest{
        Model: "auto",
        Messages: []korai.Message{{Role: "user", Content: "stream me"}},
    })
    if err != nil {
        panic(err)
    }
    for ev := range events {
        if ev.Type == "content" {
            io.WriteString(os.Stdout, ev.Delta)
        }
    }
}
```

A runnable example lives in `examples/quickstart/`:

```bash
cd examples/quickstart
go run main.go --prompt "Explique-moi la TVA suisse en 3 phrases."
```

## Authentication

```go
cli := korai.New()
tp, err := cli.Login(ctx, "user@example.com", "secret-password")
if err != nil {
    if errors.Is(err, korai.ErrUnauthorized) {
        // bad credentials
    }
}
// `cli` now carries the token; subsequent calls are authenticated.
fmt.Println(tp.AccessToken)
```

`ParseJWT` decodes the embedded claims **without** verifying the
signature — useful for client-side display only. Always rely on the
orchestrator (`/auth/me`) for trust decisions.

## Tools

The Go registry is intentionally simpler than the Python class
hierarchy: implement the four-method `Tool` interface, register it,
and consume the schemas in either OpenAI or Anthropic format.

```go
type WeatherTool struct{}

func (WeatherTool) Name() string        { return "get_weather" }
func (WeatherTool) Description() string { return "Get today's weather for a city" }
func (WeatherTool) InputSchema() any {
    return map[string]any{
        "type": "object",
        "properties": map[string]any{
            "city": map[string]any{"type": "string"},
        },
        "required": []string{"city"},
    }
}
func (WeatherTool) Execute(ctx context.Context, input json.RawMessage) (*korai.ToolResult, error) {
    return &korai.ToolResult{Output: "sunny, 23°C"}, nil
}

cli.Tools.Register(WeatherTool{})
schemas := cli.Tools.ToOpenAISchemas() // or ToAnthropicSchemas
```

For a one-off / inline tool use `korai.FuncTool`.

## Audit

`korai.Audit` ships with an in-memory store sufficient for tests and
single-process services. Production callers swap it out via
`WithAuditStore`:

```go
type postgresStore struct{ /* ... */ }

func (postgresStore) Append(...) error { /* ... */ }
// ... other methods of the AuditStore interface

cli := korai.New(korai.WithAuditStore(postgresStore{}))
entry, err := cli.Audit.Log(ctx, korai.AuditEntry{
    EventType:      "vat_recompute",
    OrganizationID: "org-1",
    UserID:         "u1",
    Payload:        map[string]any{"period": "Q1-2026"},
})
ok, count, _ := cli.Audit.VerifyChain(ctx, "org-1")
```

The chain hash format is byte-compatible with
`vertical-fiduciaire/backend/services/audit.py` so entries logged
from either SDK can be replayed against the other for verification.

## Errors

Every method returns either:

- a typed value, or
- a plain Go `error` that may be either a `*korai.APIError` or a
  wrapper around the underlying transport failure.

Match common HTTP failures with `errors.Is`:

```go
if errors.Is(err, korai.ErrUnauthorized) { /* re-login */ }
if errors.Is(err, korai.ErrRateLimited)  { /* back off */ }
if errors.Is(err, korai.ErrNotFound)     { /* missing resource */ }
```

For full structure (status code, server-side error type, retry-after,
body details) type-assert to `*korai.APIError`.

## Comparison with sdk-py / sdk-js

| Concept                | sdk-py                                | sdk-js                              | sdk-go                              |
|------------------------|---------------------------------------|-------------------------------------|-------------------------------------|
| Top-level type         | `KoraiClient` (lazy modules)          | `KoraiClient` (eager modules)       | `Client` (flat methods)             |
| Construction           | kwargs + httpx async                  | options object + fetch              | functional options + net/http       |
| Auth                   | `client.auth.login()`                 | `client.auth.login()`               | `cli.Login()`                       |
| Chat                   | `client.llm.complete()`               | `client.llm.complete()`             | `cli.ChatComplete()`                |
| Streaming              | `async for ev in client.llm.stream()` | `for await (ev of client.llm.stream())` | `for ev := range cli.ChatStream()` |
| Tools                  | abstract base class + Pydantic models | (stub today)                        | `Tool` interface, `FuncTool` helper |
| Audit storage          | server-side (cloud API)               | server-side                         | local `AuditStore` + chained hashes |
| HTTP error             | `KoraiAPIError` + subclasses          | `KoraiAPIError`                     | `*APIError` + sentinel `errors.Is`  |
| Async model            | `async`/`await`                       | `Promise`                           | `context.Context` + goroutines      |
| Dependencies           | `httpx`, `pydantic`                   | none (uses `fetch`)                 | none (stdlib only)                  |

The Go SDK is deliberately flatter — `cli.ChatComplete` instead of
`cli.LLM.Complete` — because Go's lack of method shadowing makes
property-style nested namespaces feel awkward. Tools and Audit are
exposed as fields (`cli.Tools`, `cli.Audit`) because they have
useful methods of their own and the field-style access matches Go
idioms (`http.DefaultClient`, `slog.Default()`).

## Testing

```bash
cd korai-platform/packages/sdk-go
go mod tidy
go test ./...
```

Tests use `httptest.Server` and never touch the network; they're
safe to run in CI.

## Contributing

The SDK is generated by hand and reviewed against the Python and JS
counterparts so the seven-module identity is preserved across
languages. When adding a method:

1. Mirror the type names from `sdk-py` (just rename to Go style).
2. Add a unit test using `httptest.Server`.
3. Document the corresponding orchestrator endpoint in the godoc.

## License

Same as the parent `korai-platform` repository.

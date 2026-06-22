package korai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Mode is a logical model alias the orchestrator resolves at request
// time against the models connected workers advertise. Pass one as
// ChatRequest.Model, or call ListModes for the live set + descriptions.
type Mode string

const (
	ModeAuto     Mode = "auto"
	ModeFast     Mode = "fast"
	ModeBalanced Mode = "balanced"
	ModeDeep     Mode = "deep"
)

// Modes lists the known modes in display order (auto first).
var Modes = []Mode{ModeAuto, ModeFast, ModeBalanced, ModeDeep}

// Message is a single turn in a chat conversation. The Role is one of
// "system" / "user" / "assistant" / "tool". Name is required for
// role=tool to identify which tool produced the message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
	// ToolCalls holds tool invocations requested by the model on an
	// assistant message. Parsed from the OpenAI function-call shape.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a role="tool" result message back to the call
	// that produced it, for the tool-use round trip.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall is a normalised tool invocation requested by the model.
// It unmarshals both the OpenAI function-call shape
// ({id,type,function:{name,arguments:"<json>"}}) and a pre-structured
// shape ({id,name,input}); see UnmarshalJSON.
type ToolCall struct {
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

// UnmarshalJSON normalises both wire shapes into ToolCall. OpenAI
// encodes arguments as a JSON *string*; a structured emitter may send
// an object. Unparseable arguments degrade to a nil map rather than
// failing the whole decode.
func (tc *ToolCall) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	tc.ID = raw.ID
	if raw.Function.Name != "" {
		tc.Name = raw.Function.Name
		tc.Input = decodeToolArgs(raw.Function.Arguments)
	} else {
		tc.Name = raw.Name
		tc.Input = decodeToolArgs(raw.Input)
	}
	return nil
}

// decodeToolArgs handles arguments arriving either as a JSON-encoded
// string (OpenAI) or as a raw JSON object. Returns nil on anything
// it can't parse into an object.
func decodeToolArgs(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		var m map[string]any
		if json.Unmarshal([]byte(asString), &m) == nil {
			return m
		}
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	return nil
}

// ChatRequest is the body of a /v1/chat/completions call. Mirrors
// the orchestrator's openAIChatRequest type; unknown fields are
// silently ignored on the server side, so you can extend this struct
// downstream without breaking the wire contract.
//
// Tools holds tool descriptors in either OpenAI or Anthropic schema
// shape — typically obtained via cli.Tools.ToOpenAISchemas() or
// ToAnthropicSchemas(). It is `[]any` to keep the SDK agnostic to
// which provider the orchestrator routes to.
type ChatRequest struct {
	Messages    []Message `json:"messages"`
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	TopP        float64   `json:"top_p,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	Tools       []any     `json:"tools,omitempty"`
	System      string    `json:"system,omitempty"`
	// Web enables Phase 7 web tools (search + fetch) for the model.
	// Note: when SearXNG is configured server-side, all requests go
	// through the tool loop regardless of this flag.
	Web bool `json:"web,omitempty"`
}

// Choice is one of the choices in a non-streaming completion
// response.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage reports token accounting for a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse mirrors the OpenAI-compatible response envelope.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
	// Attribution is set when the orchestrator's tool loop ran
	// (web search / fetch). Available on non-streaming responses
	// and on the final SSE chunk.
	Attribution map[string]any `json:"attribution,omitempty"`
}

// StreamEvent is a single SSE chunk normalised for Go consumers.
// Type is one of:
//
//	content   — Delta carries appended assistant text
//	tool_use  — ToolUse carries a tool invocation request
//	error     — Error carries a server-side failure
//	done      — sentinel emitted when the stream finishes
type StreamEvent struct {
	Type    string        `json:"type"`
	Delta   string        `json:"delta,omitempty"`
	ToolUse *ToolUseEvent `json:"tool_use,omitempty"`
	Error   string        `json:"error,omitempty"`
	Done    bool          `json:"done,omitempty"`
	// Raw is the unmodified envelope decoded from the SSE chunk,
	// available so callers can opt into fields the SDK doesn't
	// surface yet (attribution, finish_reason, …).
	Raw map[string]any `json:"raw,omitempty"`
}

// ToolUseEvent represents a tool invocation requested by the model.
// The orchestrator currently fences tools as in-band text, but the
// type is here so future structured tool-use fits without an API
// break.
type ToolUseEvent struct {
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

// ChatComplete sends a non-streaming chat completion request. Stream
// is forced to false so the orchestrator returns a single JSON body.
// Use ChatStream for SSE.
func (c *Client) ChatComplete(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: messages must be non-empty", ErrInvalidConfig)
	}
	req.Stream = false
	if req.Model == "" {
		req.Model = "auto"
	}
	// Route through the generated client. The SDK ChatRequest carries
	// fields the spec does not model (System, Tools, per-message Name);
	// marshalling it ourselves and using the WithBody variant preserves
	// them on the wire while the generated client owns URL + transport.
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("korai: marshal request body: %w", err)
	}
	httpResp, err := c.gen.CreateChatCompletionWithBody(ctx, "application/json", bytes.NewReader(raw))
	var out ChatResponse
	if err := c.genDecode(httpResp, err, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CountTokensRequest is the body of a /v1/count_tokens call. It is a
// strict subset of ChatRequest — sampling params don't affect
// prompt-token counts, so only model + messages are accepted. Reuses
// the SDK's Message type.
type CountTokensRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

// TokenCount is the response from /v1/count_tokens: the exact number
// of prompt tokens the messages occupy under the resolved model's own
// tokenizer and chat template (not an estimate). Model echoes the
// alias/id the caller requested; ResolvedModel is the canonical model
// the count was computed against.
type TokenCount struct {
	Object        string `json:"object"`
	Model         string `json:"model"`
	ResolvedModel string `json:"resolved_model,omitempty"`
	PromptTokens  int    `json:"prompt_tokens"`
}

// CountTokens returns the exact prompt-token count for the given
// messages under the hosting model's own tokenizer and chat template.
// The model alias is resolved the same way as ChatComplete, so the
// count reflects the model that would actually serve the request. This
// performs no billable generation. Mirrors ChatComplete's transport:
// routes through the generated client and decodes the JSON body.
func (c *Client) CountTokens(ctx context.Context, req CountTokensRequest) (*TokenCount, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: messages must be non-empty", ErrInvalidConfig)
	}
	if req.Model == "" {
		req.Model = "auto"
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("korai: marshal request body: %w", err)
	}
	httpResp, err := c.gen.CountTokensWithBody(ctx, "application/json", bytes.NewReader(raw))
	var out TokenCount
	if err := c.genDecode(httpResp, err, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChatStream opens an SSE stream and returns a channel that emits
// one StreamEvent per chunk. The channel is closed when the stream
// finishes (cleanly or on error). Cancel via ctx to abort early —
// the underlying HTTP request is bound to the context and will be
// aborted, telling the orchestrator to cancel the inference.
//
// The returned error is non-nil only when the request couldn't be
// opened (transport error, 4xx/5xx body); per-event errors arrive
// inside the channel as type="error".
//
// Example:
//
//	events, err := cli.ChatStream(ctx, req)
//	if err != nil {
//	    return err
//	}
//	for ev := range events {
//	    switch ev.Type {
//	    case "content":
//	        io.WriteString(os.Stdout, ev.Delta)
//	    case "error":
//	        return errors.New(ev.Error)
//	    }
//	}
func (c *Client) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: messages must be non-empty", ErrInvalidConfig)
	}
	req.Stream = true
	if req.Model == "" {
		req.Model = "auto"
	}
	resp, err := c.doRawRequest(ctx, "POST", "/v1/chat/completions", req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("korai: unexpected status %d on stream open", resp.StatusCode)
	}

	ch := make(chan StreamEvent, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		err := parseSSEStream(ctx, resp.Body, ch)
		if err != nil && !errors.Is(err, context.Canceled) {
			// Attempt to surface a final error event. Best-effort:
			// if the channel is full and ctx already cancelled,
			// this select bails out without leaking the goroutine.
			select {
			case ch <- StreamEvent{Type: "error", Error: err.Error()}:
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}

// ChatStreamComplete opens an SSE stream and folds it into a single
// *ChatResponse — convenience for callers that want streaming on the
// wire (first-chunk latency, server-side cancellation on ctx) but a
// non-streaming result shape. It is ChatStream followed by
// CollectStream; a server-side error surfaced as a stream error event
// is returned as a non-nil error alongside the partial response.
func (c *Client) ChatStreamComplete(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	events, err := c.ChatStream(ctx, req)
	if err != nil {
		return nil, err
	}
	return CollectStream(events)
}

// modelDescriptor is the per-row payload returned by /v1/models.
// Most callers want the IDs; the rest is exposed for the dashboard
// model picker.
type modelDescriptor struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	OwnedBy       string `json:"owned_by"`
	Description   string `json:"description,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Family        string `json:"family,omitempty"`
	Variant       string `json:"variant,omitempty"`
	Quant         string `json:"quant,omitempty"`
	Role          string `json:"role,omitempty"`
	ContextLen    int    `json:"context_len,omitempty"`
	SupportsTools bool   `json:"supports_tools,omitempty"`
}

// ModelInfo is the SDK-friendly view returned by ListModels. Wraps
// the raw orchestrator response while staying open for extension.
type ModelInfo struct {
	ID            string `json:"id"`
	Kind          string `json:"kind,omitempty"`
	Family        string `json:"family,omitempty"`
	Variant       string `json:"variant,omitempty"`
	Quant         string `json:"quant,omitempty"`
	Role          string `json:"role,omitempty"`
	ContextLen    int    `json:"context_len,omitempty"`
	SupportsTools bool   `json:"supports_tools,omitempty"`
	Description   string `json:"description,omitempty"`
}

// ListModels enumerates every model currently routable on Korai
// Cloud (logical aliases + canonical worker-advertised IDs). The
// returned slice contains only the IDs; for full descriptors call
// ListModelsDetailed.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	models, err := c.ListModelsDetailed(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids, nil
}

// ListModes returns the logical modes (aliases) the orchestrator
// exposes, with their server-provided descriptions — the kind=="alias"
// subset of /v1/models. Useful for rendering a mode picker.
func (c *Client) ListModes(ctx context.Context) ([]ModelInfo, error) {
	all, err := c.ListModelsDetailed(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(Modes))
	for _, m := range all {
		if m.Kind == "alias" {
			out = append(out, m)
		}
	}
	return out, nil
}

// ListModelsDetailed is ListModels with the rich descriptor
// retained.
func (c *Client) ListModelsDetailed(ctx context.Context) ([]ModelInfo, error) {
	httpResp, err := c.gen.ListModels(ctx)
	var decoded struct {
		Object string            `json:"object"`
		Data   []modelDescriptor `json:"data"`
	}
	if err := c.genDecode(httpResp, err, &decoded); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, len(decoded.Data))
	for i, m := range decoded.Data {
		out[i] = ModelInfo{
			ID:            m.ID,
			Kind:          m.Kind,
			Family:        m.Family,
			Variant:       m.Variant,
			Quant:         m.Quant,
			Role:          m.Role,
			ContextLen:    m.ContextLen,
			SupportsTools: m.SupportsTools,
			Description:   m.Description,
		}
	}
	return out, nil
}

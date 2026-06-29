package korai

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Tool is the interface every Korai SDK tool implements. Tools are
// small Go-native pieces of logic the LLM can invoke during a
// conversation. The registry exposes them in OpenAI / Anthropic
// schemas and dispatches calls.
//
// Implementations should be cheap to construct — the registry holds
// a single instance per name and reuses it across goroutines, so
// Execute MUST be safe for concurrent use.
type Tool interface {
	// Name is the canonical identifier ("get_weather", "lookup_law"…).
	// Conventionally lowercase snake_case.
	Name() string
	// Description is a short imperative sentence consumed by the LLM.
	// Keep it under 100 chars for token economy.
	Description() string
	// InputSchema returns a JSON-schema-compatible map describing
	// the expected input. The registry serialises it directly into
	// the OpenAI / Anthropic envelope.
	InputSchema() any
	// Execute runs the tool. The input is a raw JSON message lifted
	// from the model's tool_use call; tools are responsible for
	// validating and decoding it.
	Execute(ctx context.Context, input json.RawMessage) (*ToolResult, error)
}

// ToolResult is what Execute returns. Citations / CalculationSteps /
// Confidence mirror the audit-trail fields the Python SDK exposes so
// downstream consumers can present a transparent result page.
type ToolResult struct {
	Output           any      `json:"output"`
	Citations        []string `json:"citations,omitempty"`
	CalculationSteps []string `json:"calculation_steps,omitempty"`
	Confidence       float64  `json:"confidence,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

// AnthropicTool is the schema shape /v1/messages expects for tools.
type AnthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

// OpenAITool is the schema shape /v1/chat/completions expects when
// `tools=[...]` is supplied on the request.
type OpenAITool struct {
	Type     string             `json:"type"`
	Function OpenAIToolFunction `json:"function"`
}

// OpenAIToolFunction is the inner descriptor for an OpenAI tool.
type OpenAIToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// ToolRegistry is a goroutine-safe map of tools by name, with helper
// methods to render the registered set into the schemas expected by
// the OpenAI / Anthropic tool-use APIs and to dispatch invocations.
//
// Construct one with NewToolRegistry. The Client embeds a registry
// at .Tools but the type can also be used standalone.
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewToolRegistry returns an empty registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]Tool)}
}

// Register adds a tool to the registry. Re-registering the same name
// overwrites the previous entry — useful for hot-reloading in dev.
func (r *ToolRegistry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	r.tools[t.Name()] = t
}

// Get fetches a tool by name. The boolean indicates presence,
// matching the idiomatic Go map-lookup pattern.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List returns every registered tool, sorted by name for stable
// rendering in UIs and tests.
func (r *ToolRegistry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Names returns just the names (sorted).
func (r *ToolRegistry) Names() []string {
	tools := r.List()
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name()
	}
	return names
}

// Invoke runs the named tool. Returns ErrNotFound (wrapped) when the
// name is unknown. The tool's Execute is responsible for validating
// input — Invoke just passes it through.
func (r *ToolRegistry) Invoke(ctx context.Context, name string, input json.RawMessage) (*ToolResult, error) {
	t, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("%w: tool %q not registered", ErrNotFound, name)
	}
	return t.Execute(ctx, input)
}

// ToAnthropicSchemas renders the registry into the format expected
// by Anthropic's /v1/messages tools field.
func (r *ToolRegistry) ToAnthropicSchemas() []AnthropicTool {
	tools := r.List()
	out := make([]AnthropicTool, len(tools))
	for i, t := range tools {
		out[i] = AnthropicTool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		}
	}
	return out
}

// ToOpenAISchemas renders the registry into the format expected by
// OpenAI's chat-completions tools field.
func (r *ToolRegistry) ToOpenAISchemas() []OpenAITool {
	tools := r.List()
	out := make([]OpenAITool, len(tools))
	for i, t := range tools {
		out[i] = OpenAITool{
			Type: "function",
			Function: OpenAIToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.InputSchema(),
			},
		}
	}
	return out
}

// FuncTool is a small helper that turns an inline function into a
// Tool implementation. Useful for one-off / test tools without
// having to write a struct.
//
// Example:
//
//	echo := korai.FuncTool{
//	    NameValue: "echo",
//	    DescValue: "Echo the input back",
//	    Schema:    map[string]any{"type": "object"},
//	    Fn: func(ctx context.Context, input json.RawMessage) (*korai.ToolResult, error) {
//	        return &korai.ToolResult{Output: string(input)}, nil
//	    },
//	}
type FuncTool struct {
	NameValue string
	DescValue string
	Schema    any
	Fn        func(ctx context.Context, input json.RawMessage) (*ToolResult, error)
}

// Name implements Tool.
func (f FuncTool) Name() string { return f.NameValue }

// Description implements Tool.
func (f FuncTool) Description() string { return f.DescValue }

// InputSchema implements Tool.
func (f FuncTool) InputSchema() any {
	if f.Schema == nil {
		return map[string]any{"type": "object"}
	}
	return f.Schema
}

// Execute implements Tool.
func (f FuncTool) Execute(ctx context.Context, input json.RawMessage) (*ToolResult, error) {
	if f.Fn == nil {
		return nil, fmt.Errorf("korai: FuncTool %q has nil Fn", f.NameValue)
	}
	return f.Fn(ctx, input)
}

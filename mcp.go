package korai

import (
	"context"
	"encoding/json"
	"fmt"
)

// MCPToolDef is a single tool definition as advertised by a Model
// Context Protocol (MCP) server. InputSchema is a raw JSON-schema map
// that the SDK forwards verbatim into the OpenAI / Anthropic tool
// envelope — no transformation is applied.
type MCPToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// MCPSession is the narrow, duck-typed interface the SDK needs from a
// connected MCP client session. It deliberately does NOT depend on any
// MCP SDK: adapt a real client (e.g. github.com/modelcontextprotocol/...)
// with a tiny shim that satisfies these two methods.
//
//	type shim struct{ sess *mcpsdk.ClientSession }
//	func (s shim) ListTools(ctx context.Context) ([]korai.MCPToolDef, error) { ... }
//	func (s shim) CallTool(ctx context.Context, name string, args map[string]any) (string, error) { ... }
type MCPSession interface {
	// ListTools returns the tools the connected server exposes.
	ListTools(ctx context.Context) ([]MCPToolDef, error)
	// CallTool invokes a server-side tool by name with decoded args and
	// returns its textual result.
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
}

// mcpTool adapts an MCP tool definition + session to the SDK's Tool
// interface so it slots into a ToolRegistry like any native tool.
type mcpTool struct {
	def     MCPToolDef
	regName string
	session MCPSession
}

// Name implements Tool. It returns the (optionally namespaced) name the
// tool was registered under so registry lookups and model tool_call
// dispatch line up.
func (t *mcpTool) Name() string { return t.regName }

// Description implements Tool.
func (t *mcpTool) Description() string { return t.def.Description }

// InputSchema implements Tool. The MCP-provided JSON schema is returned
// verbatim so ToOpenAISchemas / ToAnthropicSchemas emit it unchanged.
func (t *mcpTool) InputSchema() any {
	if t.def.InputSchema == nil {
		return map[string]any{"type": "object"}
	}
	return t.def.InputSchema
}

// Execute implements Tool. It decodes the model's raw JSON input into a
// map and forwards it to the MCP server's CallTool, wrapping the textual
// result in a ToolResult.
func (t *mcpTool) Execute(ctx context.Context, input json.RawMessage) (*ToolResult, error) {
	args := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, fmt.Errorf("korai: decode MCP tool %q input: %w", t.regName, err)
		}
	}
	// CallTool uses the server-side (un-namespaced) name.
	result, err := t.session.CallTool(ctx, t.def.Name, args)
	if err != nil {
		return nil, fmt.Errorf("korai: MCP tool %q call: %w", t.regName, err)
	}
	return &ToolResult{Output: result}, nil
}

// mcpRegisterConfig holds resolved MCPRegisterOption state.
type mcpRegisterConfig struct {
	namespace string
}

// MCPRegisterOption configures RegisterMCPServer.
type MCPRegisterOption func(*mcpRegisterConfig)

// WithMCPNamespace prefixes every registered MCP tool name with the
// given string. Use it to avoid collisions when registering multiple
// MCP servers (or an MCP server alongside native tools) whose tool
// names would otherwise clash — registry registration is last-writer-
// wins, so an un-namespaced collision silently overwrites. The prefix
// is applied to the registry name only; the original name is still used
// when calling back into the MCP server.
func WithMCPNamespace(prefix string) MCPRegisterOption {
	return func(cfg *mcpRegisterConfig) { cfg.namespace = prefix }
}

// RegisterMCPServer lists the tools exposed by a connected MCP session
// and registers each one into c.Tools, so a subsequent RunTools call can
// surface them to the model and dispatch tool_calls back to the MCP
// server. It returns the registered (namespaced) names in registry
// order, or any error from ListTools.
//
// Registration is last-writer-wins (see ToolRegistry.Register): if two
// MCP tools — or an MCP tool and a native tool — share a name, the later
// one overwrites the earlier. Pass WithMCPNamespace to disambiguate.
//
// There is no hard dependency on any MCP SDK: session only needs to
// satisfy the narrow MCPSession interface.
func (c *Client) RegisterMCPServer(ctx context.Context, session MCPSession, opts ...MCPRegisterOption) ([]string, error) {
	cfg := mcpRegisterConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	defs, err := session.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("korai: list MCP tools: %w", err)
	}

	names := make([]string, 0, len(defs))
	for _, def := range defs {
		regName := cfg.namespace + def.Name
		c.Tools.Register(&mcpTool{
			def:     def,
			regName: regName,
			session: session,
		})
		names = append(names, regName)
	}
	return names, nil
}

package korai

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeMCPSession is a canned MCPSession for tests. It records the last
// CallTool invocation so we can assert routing.
type fakeMCPSession struct {
	defs       []MCPToolDef
	lastName   string
	lastArgs   map[string]any
	callResult string
}

func (f *fakeMCPSession) ListTools(_ context.Context) ([]MCPToolDef, error) {
	return f.defs, nil
}

func (f *fakeMCPSession) CallTool(_ context.Context, name string, args map[string]any) (string, error) {
	f.lastName = name
	f.lastArgs = args
	if f.callResult != "" {
		return f.callResult, nil
	}
	return "called " + name, nil
}

func newFakeSession() *fakeMCPSession {
	return &fakeMCPSession{
		defs: []MCPToolDef{{
			Name:        "search_docs",
			Description: "Search the docs",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
				"required": []any{"query"},
			},
		}},
		callResult: "doc result",
	}
}

func TestRegisterMCPServerRegistersTools(t *testing.T) {
	cli := New()
	sess := newFakeSession()

	names, err := cli.RegisterMCPServer(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "search_docs" {
		t.Fatalf("names = %#v", names)
	}
	if _, ok := cli.Tools.Get("search_docs"); !ok {
		t.Fatal("search_docs not registered in c.Tools")
	}
}

func TestRegisterMCPServerEmitsVerbatimSchema(t *testing.T) {
	cli := New()
	if _, err := cli.RegisterMCPServer(context.Background(), newFakeSession()); err != nil {
		t.Fatal(err)
	}

	schemas := cli.Tools.ToOpenAISchemas()
	if len(schemas) != 1 {
		t.Fatalf("schemas = %#v", schemas)
	}
	s := schemas[0]
	if s.Function.Name != "search_docs" || s.Function.Description != "Search the docs" {
		t.Fatalf("schema func = %#v", s.Function)
	}
	params, ok := s.Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("parameters not a map: %T", s.Function.Parameters)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok || props["query"] == nil {
		t.Fatalf("schema not forwarded verbatim: %#v", params)
	}
}

func TestRegisterMCPServerInvokeRoutesToSession(t *testing.T) {
	cli := New()
	sess := newFakeSession()
	if _, err := cli.RegisterMCPServer(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	res, err := cli.Tools.Invoke(context.Background(), "search_docs", json.RawMessage(`{"query":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "doc result" {
		t.Fatalf("output = %v", res.Output)
	}
	// CallTool must receive the server-side name and decoded args.
	if sess.lastName != "search_docs" {
		t.Fatalf("lastName = %q", sess.lastName)
	}
	if sess.lastArgs["query"] != "hello" {
		t.Fatalf("lastArgs = %#v", sess.lastArgs)
	}
}

func TestRegisterMCPServerNamespacePrefix(t *testing.T) {
	cli := New()
	sess := newFakeSession()

	names, err := cli.RegisterMCPServer(context.Background(), sess, WithMCPNamespace("mcp_"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "mcp_search_docs" {
		t.Fatalf("names = %#v", names)
	}
	if _, ok := cli.Tools.Get("mcp_search_docs"); !ok {
		t.Fatal("namespaced tool not registered")
	}
	// Invoking the namespaced tool must still call the un-namespaced
	// server-side name.
	if _, err := cli.Tools.Invoke(context.Background(), "mcp_search_docs", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if sess.lastName != "search_docs" {
		t.Fatalf("lastName = %q, want un-namespaced search_docs", sess.lastName)
	}
}

// mcpAddSession mimics an MCP server exposing the "add" tool so the
// existing httptest tool-call body (which calls "add") drives an
// end-to-end RunTools loop through the MCP bridge.
type mcpAddSession struct{ called bool }

func (s *mcpAddSession) ListTools(_ context.Context) ([]MCPToolDef, error) {
	return []MCPToolDef{{
		Name:        "add",
		Description: "Add two numbers",
		InputSchema: map[string]any{"type": "object"},
	}}, nil
}

func (s *mcpAddSession) CallTool(_ context.Context, name string, args map[string]any) (string, error) {
	s.called = true
	a, _ := args["a"].(float64)
	b, _ := args["b"].(float64)
	return jsonNumber(a + b), nil
}

func jsonNumber(f float64) string {
	raw, _ := json.Marshal(f)
	return string(raw)
}

func TestRunToolsWithMCPTool(t *testing.T) {
	srv, _ := sequencedServer(t, toolCallBody, finalBody("The answer is 5."))
	cli := New(WithBaseURL(srv.URL))

	sess := &mcpAddSession{}
	if _, err := cli.RegisterMCPServer(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	res, err := cli.RunTools(context.Background(), RunToolsOptions{
		Messages: []Message{{Role: "user", Content: "what is 2+3?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sess.called {
		t.Fatal("MCP CallTool was not invoked by RunTools")
	}
	if res.Final.Choices[0].Message.Content != "The answer is 5." {
		t.Fatalf("final = %q", res.Final.Choices[0].Message.Content)
	}
	if len(res.ToolRuns) != 1 || res.ToolRuns[0].Name != "add" {
		t.Fatalf("tool runs = %#v", res.ToolRuns)
	}
}

package korai

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// echoTool is a simple Tool used by the registry tests. It echoes
// the input back as Output and tags the result with citations.
type echoTool struct{}

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "Return the input unchanged" }
func (echoTool) InputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
		"required": []string{"text"},
	}
}
func (echoTool) Execute(ctx context.Context, input json.RawMessage) (*ToolResult, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	return &ToolResult{
		Output:    args.Text,
		Citations: []string{"echo://"},
	}, nil
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewToolRegistry()
	r.Register(echoTool{})
	got, ok := r.Get("echo")
	if !ok {
		t.Fatal("expected echo registered")
	}
	if got.Name() != "echo" {
		t.Fatalf("name = %q", got.Name())
	}
	if _, ok := r.Get("unknown"); ok {
		t.Fatal("unexpected hit")
	}
}

func TestRegistryListIsSorted(t *testing.T) {
	r := NewToolRegistry()
	r.Register(FuncTool{NameValue: "zeta"})
	r.Register(FuncTool{NameValue: "alpha"})
	names := r.Names()
	if !reflect.DeepEqual(names, []string{"alpha", "zeta"}) {
		t.Fatalf("not sorted: %v", names)
	}
}

func TestRegistryInvokeRoundtrip(t *testing.T) {
	r := NewToolRegistry()
	r.Register(echoTool{})
	res, err := r.Invoke(context.Background(), "echo", json.RawMessage(`{"text":"yo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "yo" {
		t.Fatalf("output = %v", res.Output)
	}
	if len(res.Citations) != 1 || res.Citations[0] != "echo://" {
		t.Fatalf("citations = %v", res.Citations)
	}
}

func TestRegistryInvokeUnknownReturnsNotFound(t *testing.T) {
	r := NewToolRegistry()
	_, err := r.Invoke(context.Background(), "missing", json.RawMessage(`{}`))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRegistryAnthropicSchemas(t *testing.T) {
	r := NewToolRegistry()
	r.Register(echoTool{})
	schemas := r.ToAnthropicSchemas()
	if len(schemas) != 1 {
		t.Fatalf("len = %d", len(schemas))
	}
	if schemas[0].Name != "echo" || schemas[0].Description == "" {
		t.Fatalf("bad schema: %#v", schemas[0])
	}
	if schemas[0].InputSchema == nil {
		t.Fatal("missing schema map")
	}
}

func TestRegistryOpenAISchemas(t *testing.T) {
	r := NewToolRegistry()
	r.Register(echoTool{})
	schemas := r.ToOpenAISchemas()
	if len(schemas) != 1 {
		t.Fatalf("len = %d", len(schemas))
	}
	if schemas[0].Type != "function" {
		t.Fatalf("type = %q", schemas[0].Type)
	}
	if schemas[0].Function.Name != "echo" {
		t.Fatalf("name = %q", schemas[0].Function.Name)
	}
}

func TestFuncToolWithoutFn(t *testing.T) {
	tool := FuncTool{NameValue: "broken"}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFuncToolHappyPath(t *testing.T) {
	called := false
	tool := FuncTool{
		NameValue: "ping",
		DescValue: "ping",
		Schema:    map[string]any{"type": "object"},
		Fn: func(ctx context.Context, input json.RawMessage) (*ToolResult, error) {
			called = true
			return &ToolResult{Output: "pong"}, nil
		},
	}
	r := NewToolRegistry()
	r.Register(tool)
	res, err := r.Invoke(context.Background(), "ping", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !called || res.Output != "pong" {
		t.Fatalf("not called or wrong output: %v", res)
	}
}

func TestRegistryReplaceOnReregister(t *testing.T) {
	r := NewToolRegistry()
	r.Register(FuncTool{NameValue: "x", DescValue: "first"})
	r.Register(FuncTool{NameValue: "x", DescValue: "second"})
	got, _ := r.Get("x")
	if got.Description() != "second" {
		t.Fatalf("expected replacement, got %q", got.Description())
	}
}

func TestFuncToolDefaultSchema(t *testing.T) {
	tool := FuncTool{
		NameValue: "noop",
		Fn: func(context.Context, json.RawMessage) (*ToolResult, error) {
			return &ToolResult{Output: nil}, nil
		},
	}
	if tool.InputSchema() == nil {
		t.Fatal("expected default schema")
	}
}

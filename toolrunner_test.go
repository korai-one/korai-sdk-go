package korai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// addTool sums a.b for the tool-runner tests.
type addTool struct{}

func (addTool) Name() string        { return "add" }
func (addTool) Description() string { return "Add two numbers" }
func (addTool) InputSchema() any    { return map[string]any{"type": "object"} }
func (addTool) Execute(_ context.Context, input json.RawMessage) (*ToolResult, error) {
	var args struct {
		A float64 `json:"a"`
		B float64 `json:"b"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	return &ToolResult{Output: args.A + args.B}, nil
}

const toolCallBody = `{"id":"1","object":"chat.completion","model":"auto",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"",` +
	`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"add","arguments":"{\"a\":2,\"b\":3}"}}]},` +
	`"finish_reason":"tool_calls"}]}`

func finalBody(content string) string {
	return `{"id":"2","object":"chat.completion","model":"auto",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}]}`
}

// sequencedServer returns the supplied bodies in order (last one repeats),
// recording each request body it received.
func sequencedServer(t *testing.T, bodies ...string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(raw))
		body := bodies[i]
		if i < len(bodies)-1 {
			i++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestRunToolsExecutesThenCompletes(t *testing.T) {
	srv, reqs := sequencedServer(t, toolCallBody, finalBody("The answer is 5."))
	cli := New(WithBaseURL(srv.URL))
	cli.Tools.Register(addTool{})

	res, err := cli.RunTools(context.Background(), RunToolsOptions{
		Messages: []Message{{Role: "user", Content: "what is 2+3?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Turns != 2 {
		t.Fatalf("turns = %d, want 2", res.Turns)
	}
	if res.StoppedAtMaxTurns {
		t.Fatal("unexpected StoppedAtMaxTurns")
	}
	if res.Final.Choices[0].Message.Content != "The answer is 5." {
		t.Fatalf("final = %q", res.Final.Choices[0].Message.Content)
	}
	if len(res.ToolRuns) != 1 || res.ToolRuns[0].Name != "add" {
		t.Fatalf("tool runs = %#v", res.ToolRuns)
	}
	if !strings.Contains(string(res.ToolRuns[0].Output), "5") {
		t.Fatalf("tool output = %s", res.ToolRuns[0].Output)
	}
	// The second request must carry the tool result back to the model.
	if len(*reqs) != 2 {
		t.Fatalf("got %d requests", len(*reqs))
	}
	var second struct {
		Messages []Message `json:"messages"`
	}
	_ = json.Unmarshal([]byte((*reqs)[1]), &second)
	var toolMsg *Message
	for i := range second.Messages {
		if second.Messages[i].Role == "tool" {
			toolMsg = &second.Messages[i]
		}
	}
	if toolMsg == nil || toolMsg.ToolCallID != "call_1" {
		t.Fatalf("tool message not fed back: %#v", second.Messages)
	}
}

func TestRunToolsFeedsToolErrorBack(t *testing.T) {
	// Model calls a tool that isn't registered → error fed back, loop continues.
	srv, _ := sequencedServer(t, toolCallBody, finalBody("sorry"))
	cli := New(WithBaseURL(srv.URL)) // no tool registered
	res, err := cli.RunTools(context.Background(), RunToolsOptions{
		Messages: []Message{{Role: "user", Content: "add"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolRuns) != 1 || !strings.Contains(string(res.ToolRuns[0].Output), "error") {
		t.Fatalf("expected error fed back, got %#v", res.ToolRuns)
	}
	if res.Final.Choices[0].Message.Content != "sorry" {
		t.Fatalf("final = %q", res.Final.Choices[0].Message.Content)
	}
}

func TestRunToolsStopsAtMaxTurns(t *testing.T) {
	srv, _ := sequencedServer(t, toolCallBody) // always asks for a tool
	cli := New(WithBaseURL(srv.URL))
	cli.Tools.Register(addTool{})
	res, err := cli.RunTools(context.Background(), RunToolsOptions{
		Messages: []Message{{Role: "user", Content: "loop"}},
		MaxTurns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Turns != 3 || !res.StoppedAtMaxTurns {
		t.Fatalf("turns = %d, stopped = %v", res.Turns, res.StoppedAtMaxTurns)
	}
}

func TestToolCallUnmarshal(t *testing.T) {
	// OpenAI shape with arguments as a JSON string.
	var openai ToolCall
	if err := json.Unmarshal([]byte(`{"id":"c1","type":"function","function":{"name":"add","arguments":"{\"a\":2}"}}`), &openai); err != nil {
		t.Fatal(err)
	}
	if openai.Name != "add" || openai.Input["a"] != float64(2) {
		t.Fatalf("openai shape = %#v", openai)
	}
	// Pre-structured shape with an object input.
	var structured ToolCall
	if err := json.Unmarshal([]byte(`{"id":"c2","name":"y","input":{"k":1}}`), &structured); err != nil {
		t.Fatal(err)
	}
	if structured.Name != "y" || structured.Input["k"] != float64(1) {
		t.Fatalf("structured shape = %#v", structured)
	}
	// Unparseable arguments degrade to a nil map (no error).
	var bad ToolCall
	if err := json.Unmarshal([]byte(`{"function":{"name":"z","arguments":"not json"}}`), &bad); err != nil {
		t.Fatal(err)
	}
	if bad.Name != "z" || bad.Input != nil {
		t.Fatalf("bad args = %#v", bad)
	}
}

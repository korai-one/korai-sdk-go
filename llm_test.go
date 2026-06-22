package korai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatCompleteHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req ChatRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Model != "auto" || len(req.Messages) != 1 {
			t.Fatalf("unexpected request: %#v", req)
		}
		w.Write([]byte(`{
			"id": "chatcmpl-1",
			"object": "chat.completion",
			"created": 1730000000,
			"model": "gemma-4-31b-thinking-4bit",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "hi"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	resp, err := cli.ChatComplete(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "hi" {
		t.Fatalf("content = %q", resp.Choices[0].Message.Content)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Fatalf("usage.total = %d", resp.Usage.TotalTokens)
	}
}

func TestCountTokensHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/count_tokens" {
			http.NotFound(w, r)
			return
		}
		var req CountTokensRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Model != "auto" || len(req.Messages) != 1 {
			t.Fatalf("unexpected request: %#v", req)
		}
		w.Write([]byte(`{
			"object": "token_count",
			"model": "auto",
			"resolved_model": "gemma-4-31b-thinking-4bit",
			"prompt_tokens": 42
		}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithMaxRetries(0))
	resp, err := cli.CountTokens(context.Background(), CountTokensRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Object != "token_count" {
		t.Fatalf("object = %q", resp.Object)
	}
	if resp.PromptTokens != 42 {
		t.Fatalf("prompt_tokens = %d", resp.PromptTokens)
	}
	if resp.ResolvedModel != "gemma-4-31b-thinking-4bit" {
		t.Fatalf("resolved_model = %q", resp.ResolvedModel)
	}
}

func TestCountTokensRequiresMessages(t *testing.T) {
	cli := New()
	_, err := cli.CountTokens(context.Background(), CountTokensRequest{})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestChatCompleteRequiresMessages(t *testing.T) {
	cli := New()
	_, err := cli.ChatComplete(context.Background(), ChatRequest{})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestChatCompleteForcesNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		if req.Stream {
			t.Fatalf("expected stream=false, got true")
		}
		w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"k"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	_, err := cli.ChatComplete(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListModelsParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{
			"object":"list",
			"data":[
				{"id":"auto","kind":"alias","owned_by":"korai"},
				{"id":"fast","kind":"alias","owned_by":"korai"},
				{"id":"gemma-4-31b-thinking-4bit","kind":"canonical","owned_by":"korai","family":"gemma","variant":"31b","quant":"4bit","role":"deep","context_len":131072,"supports_tools":true}
			]
		}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	ids, err := cli.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != "auto" {
		t.Fatalf("ids = %v", ids)
	}

	detailed, err := cli.ListModelsDetailed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var deep ModelInfo
	for _, m := range detailed {
		if m.Family == "gemma" {
			deep = m
		}
	}
	if !deep.SupportsTools || deep.ContextLen != 131072 {
		t.Fatalf("unexpected gemma descriptor: %#v", deep)
	}

	modes, err := cli.ListModes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(modes) != 2 {
		t.Fatalf("expected 2 alias modes, got %d: %#v", len(modes), modes)
	}
	for _, m := range modes {
		if m.Kind != "alias" {
			t.Fatalf("non-alias in modes: %#v", m)
		}
	}
}

func TestChatStreamYieldsContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"x","choices":[{"index":0,"delta":{"content":"hel"}}]}`,
			`{"id":"x","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
			`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	events, err := cli.ChatStream(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var collected strings.Builder
	saw := map[string]bool{}
	for ev := range events {
		saw[ev.Type] = true
		if ev.Type == "content" {
			collected.WriteString(ev.Delta)
		}
	}
	if collected.String() != "hello" {
		t.Fatalf("collected = %q", collected.String())
	}
	if !saw["done"] {
		t.Fatalf("did not see done event, saw: %#v", saw)
	}
}

func TestChatStreamSurfacesErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", `{"error":{"message":"worker offline","type":"server_error"}}`)
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	events, err := cli.ChatStream(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var errEvents []StreamEvent
	for ev := range events {
		if ev.Type == "error" {
			errEvents = append(errEvents, ev)
		}
	}
	if len(errEvents) == 0 || !strings.Contains(errEvents[0].Error, "worker offline") {
		t.Fatalf("expected error event, got %#v", errEvents)
	}
}

func TestChatStreamRespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// Send a single chunk then hold the connection open until the
		// client gives up.
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"x"}}]}`)
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	events, err := cli.ChatStream(ctx, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for range events {
		count++
	}
	if count == 0 {
		t.Fatalf("expected at least one event before cancel")
	}
}

func TestChatStreamCompleteConcatenates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"x","choices":[{"index":0,"delta":{"content":"hel"}}]}`,
			`{"id":"x","choices":[{"index":0,"delta":{"content":"lo wor"}}]}`,
			`{"id":"x","choices":[{"index":0,"delta":{"content":"ld"},"finish_reason":"stop"}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	resp, err := cli.ChatStreamComplete(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "hello world" {
		t.Fatalf("content = %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Fatalf("role = %q", resp.Choices[0].Message.Role)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

func TestChatStreamCompleteSurfacesErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"error":{"message":"worker offline","type":"server_error"}}`)
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	resp, err := cli.ChatStreamComplete(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from stream error event")
	}
	if !strings.Contains(err.Error(), "worker offline") {
		t.Fatalf("err = %v", err)
	}
	// Partial content collected before the error is still returned.
	if resp == nil || resp.Choices[0].Message.Content != "partial" {
		t.Fatalf("partial response = %#v", resp)
	}
}

func TestChatCompleteReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"messages must be non-empty","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	_, err := cli.ChatComplete(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if apiErr.Code != "invalid_request_error" {
		t.Fatalf("code = %q", apiErr.Code)
	}
}

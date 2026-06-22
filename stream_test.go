package korai

import (
	"context"
	"strings"
	"testing"
)

func TestParseSSEStreamBasic(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"content":"foo"}}]}

data: {"choices":[{"index":0,"delta":{"content":"bar"}}]}

data: [DONE]

`
	out := make(chan StreamEvent, 4)
	if err := parseSSEStream(context.Background(), strings.NewReader(body), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var collected strings.Builder
	sawDone := false
	for ev := range out {
		if ev.Type == "content" {
			collected.WriteString(ev.Delta)
		}
		if ev.Type == "done" {
			sawDone = true
		}
	}
	if collected.String() != "foobar" {
		t.Fatalf("collected = %q", collected.String())
	}
	if !sawDone {
		t.Fatal("expected done event")
	}
}

func TestParseSSEStreamHandlesComments(t *testing.T) {
	body := `:keepalive
data: {"choices":[{"index":0,"delta":{"content":"x"}}]}

data: [DONE]

`
	out := make(chan StreamEvent, 4)
	if err := parseSSEStream(context.Background(), strings.NewReader(body), out); err != nil {
		t.Fatal(err)
	}
	close(out)
	count := 0
	for range out {
		count++
	}
	if count != 2 { // one content + one done
		t.Fatalf("count = %d", count)
	}
}

func TestParseSSEStreamSurfacesErrors(t *testing.T) {
	body := `data: {"error":{"message":"boom","type":"server_error"}}

data: [DONE]

`
	out := make(chan StreamEvent, 4)
	if err := parseSSEStream(context.Background(), strings.NewReader(body), out); err != nil {
		t.Fatal(err)
	}
	close(out)
	saw := ""
	for ev := range out {
		if ev.Type == "error" {
			saw = ev.Error
		}
	}
	if saw != "boom" {
		t.Fatalf("error = %q", saw)
	}
}

func TestParseSSEStreamHonoursContextCancel(t *testing.T) {
	// A reader that blocks forever; ctx cancellation must unstick us.
	body := `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}

`
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan StreamEvent, 1)
	err := parseSSEStream(ctx, strings.NewReader(body), out)
	if err == nil {
		t.Fatalf("expected error from cancelled ctx")
	}
}

func TestCollectStreamConcatenates(t *testing.T) {
	in := make(chan StreamEvent, 4)
	in <- StreamEvent{Type: "content", Delta: "abc"}
	in <- StreamEvent{Type: "content", Delta: "def"}
	in <- StreamEvent{Type: "done", Done: true}
	close(in)
	got, err := CollectStream(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Choices[0].Message.Content != "abcdef" {
		t.Fatalf("content = %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].Message.Role != "assistant" {
		t.Fatalf("role = %q", got.Choices[0].Message.Role)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", got.Choices[0].FinishReason)
	}
}

func TestCollectStreamPropagatesErrorEvents(t *testing.T) {
	in := make(chan StreamEvent, 2)
	in <- StreamEvent{Type: "content", Delta: "ok"}
	in <- StreamEvent{Type: "error", Error: "network gone"}
	close(in)
	got, err := CollectStream(in)
	if err == nil {
		t.Fatal("expected error")
	}
	if got == nil || got.Choices[0].Message.Content != "ok" {
		t.Fatalf("partial = %#v", got)
	}
}

func TestDecodeChunkStatusEvent(t *testing.T) {
	ev, err := decodeChunk(`{"status":"searching"}`)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "status" || ev.Delta != "searching" {
		t.Fatalf("ev = %#v", ev)
	}
}

func TestDecodeChunkInvalidJSON(t *testing.T) {
	_, err := decodeChunk(`not json`)
	if err == nil {
		t.Fatal("expected error")
	}
}

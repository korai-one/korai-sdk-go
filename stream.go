package korai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// parseSSEStream reads a server-sent-events stream from r and emits
// normalised StreamEvent values onto out. It understands the
// orchestrator's chunked openAIStreamEnvelope plus the conventional
// "[DONE]" terminator.
//
// The function returns:
//   - nil on a clean termination (either [DONE] received or the
//     stream closed naturally),
//   - context.Canceled when ctx is cancelled,
//   - an error wrapped with the underlying io failure on transport
//     issues.
//
// The caller is responsible for closing r.
func parseSSEStream(ctx context.Context, r io.Reader, out chan<- StreamEvent) error {
	scanner := bufio.NewScanner(r)
	// Allow large chunks — some thinking tokens stream as multi-line
	// JSON, especially during tool use.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var dataBuf bytes.Buffer

	flush := func() error {
		if dataBuf.Len() == 0 {
			return nil
		}
		raw := strings.TrimSpace(dataBuf.String())
		dataBuf.Reset()

		if raw == "" {
			return nil
		}
		if raw == "[DONE]" {
			return sendEvent(ctx, out, StreamEvent{Type: "done", Done: true})
		}

		ev, err := decodeChunk(raw)
		if err != nil {
			return sendEvent(ctx, out, StreamEvent{Type: "error", Error: err.Error()})
		}
		return sendEvent(ctx, out, ev)
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()
		// SSE: blank line terminates a frame.
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		// Comment lines — ignore.
		if strings.HasPrefix(line, ":") {
			continue
		}
		// `event:` / `id:` / `retry:` lines are not used by the
		// orchestrator, but the parser must skip them gracefully.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		// Strip "data:" prefix; the spec allows an optional space.
		payload := strings.TrimPrefix(line, "data:")
		payload = strings.TrimPrefix(payload, " ")
		if dataBuf.Len() > 0 {
			dataBuf.WriteByte('\n')
		}
		dataBuf.WriteString(payload)
	}
	// Final flush in case the server didn't send a blank line.
	if err := flush(); err != nil {
		return err
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// decodeChunk turns a single SSE data payload into a normalised
// StreamEvent. The orchestrator emits OpenAI-compatible
// chat.completion.chunk envelopes plus a "status" field for tool
// loop transparency.
func decodeChunk(raw string) (StreamEvent, error) {
	var envelope struct {
		ID      string `json:"id,omitempty"`
		Object  string `json:"object,omitempty"`
		Status  string `json:"status,omitempty"`
		Choices []struct {
			Delta struct {
				Role    string `json:"role,omitempty"`
				Content string `json:"content,omitempty"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason,omitempty"`
		} `json:"choices,omitempty"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type,omitempty"`
		} `json:"error,omitempty"`
		Attribution map[string]any `json:"attribution,omitempty"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return StreamEvent{}, err
	}

	// Always preserve raw payload for callers who need
	// orchestrator-specific fields.
	rawMap := make(map[string]any)
	_ = json.Unmarshal([]byte(raw), &rawMap)

	if envelope.Error != nil {
		return StreamEvent{
			Type:  "error",
			Error: envelope.Error.Message,
			Raw:   rawMap,
		}, nil
	}

	if envelope.Status != "" && len(envelope.Choices) == 0 {
		return StreamEvent{
			Type:  "status",
			Delta: envelope.Status,
			Raw:   rawMap,
		}, nil
	}

	var ev StreamEvent
	ev.Type = "content"
	ev.Raw = rawMap
	for _, ch := range envelope.Choices {
		if ch.Delta.Content != "" {
			ev.Delta += ch.Delta.Content
		}
		if ch.FinishReason != nil {
			ev.Done = true
		}
	}
	return ev, nil
}

// sendEvent dispatches an event to the channel honouring ctx
// cancellation. Returns context.Canceled if the caller stopped
// reading.
func sendEvent(ctx context.Context, out chan<- StreamEvent, ev StreamEvent) error {
	select {
	case out <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CollectStream drains a StreamEvent channel and folds it into a final
// *ChatResponse — the Go analogue of Anthropic's stream → final message
// helper (Python: stream.get_final_message()). Content deltas are
// concatenated into Choices[0].Message.Content (role "assistant") and
// FinishReason is set to "stop". When the channel was produced by
// ChatStream, the SSE [DONE] terminator yields a type="done" event that
// is simply drained.
//
// If a type="error" event arrives, the accumulated content so far is
// returned alongside a non-nil error so callers can still inspect the
// partial message. Multiple error events are aggregated.
//
// The returned *ChatResponse is always non-nil, even on error.
func CollectStream(events <-chan StreamEvent) (*ChatResponse, error) {
	var sb strings.Builder
	var errs []string
	for ev := range events {
		switch ev.Type {
		case "content":
			sb.WriteString(ev.Delta)
		case "error":
			errs = append(errs, ev.Error)
		}
	}
	resp := &ChatResponse{
		Object: "chat.completion",
		Choices: []Choice{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: sb.String()},
			FinishReason: "stop",
		}},
	}
	if len(errs) > 0 {
		return resp, fmt.Errorf("korai: stream error: %s", strings.Join(errs, "; "))
	}
	return resp, nil
}

package korai

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestSessionJSONRoundTrip checks that a Session carrying every block type
// survives a marshal/unmarshal round trip with its concrete Block types intact.
func TestSessionJSONRoundTrip(t *testing.T) {
	orig := Session{
		ID:      "20260708-120000-abcd",
		Created: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
		Updated: time.Date(2026, 7, 8, 12, 5, 0, 0, time.UTC),
		CWD:     "/home/u/proj",
		Model:   "auto",
		Tool:    "korai-code-cli",
		Messages: []SessionMessage{
			{Role: "user", Blocks: []Block{
				TextBlock{Text: "look at this"},
				ImageBlock{Source: "data:image/png;base64,AAAA"},
			}},
			{Role: "assistant", Blocks: []Block{
				TextBlock{Text: "calling a tool"},
				ToolUseBlock{ID: "call_1", Name: "grep", Input: json.RawMessage(`{"pattern":"foo"}`)},
			}},
			{Role: "tool", Blocks: []Block{
				ToolResultBlock{ToolCallID: "call_1", Name: "grep", Content: "match", IsError: false},
			}},
		},
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Session
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("round trip mismatch:\n orig=%#v\n got =%#v", orig, got)
	}
}

// TestSessionMessageUnknownBlockSkipped verifies an unrecognized block kind is
// dropped on decode rather than breaking the whole message.
func TestSessionMessageUnknownBlockSkipped(t *testing.T) {
	raw := `{"role":"user","blocks":[{"kind":"text","text":"hi"},{"kind":"future","weird":true}]}`
	var m SessionMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.Blocks) != 1 {
		t.Fatalf("want 1 block after dropping unknown, got %d", len(m.Blocks))
	}
	if tb, ok := m.Blocks[0].(TextBlock); !ok || tb.Text != "hi" {
		t.Fatalf("unexpected surviving block: %#v", m.Blocks[0])
	}
}

// flatMessages returns representative flat wire messages that must survive
// flat -> rich -> flat losslessly.
func flatMessages() []Message {
	return []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
		UserMessageWithParts(TextPart("describe"), ImagePart("data:image/png;base64,ZZZ")),
		{Role: "assistant", Content: "sure, running tool", ToolCalls: []ToolCall{
			{ID: "call_9", Name: "search", Input: map[string]any{"q": "korai"}},
		}},
		{Role: "tool", Name: "search", Content: "results here", ToolCallID: "call_9"},
	}
}

// TestProjectionFlatRoundTripLossless asserts flat -> rich -> flat is the
// identity on representative flat inputs.
func TestProjectionFlatRoundTripLossless(t *testing.T) {
	for _, orig := range flatMessages() {
		rich := ToSessionMessage(orig)
		got := rich.ToMessage()

		// Compare via the wire JSON so equivalent representations (e.g. a parts
		// message whose Content mirrors flattened text) match as they do on the
		// wire, and map key ordering is normalized.
		wantJSON, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("marshal orig: %v", err)
		}
		gotJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal got: %v", err)
		}
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("flat->rich->flat mismatch for role=%q:\n want %s\n got  %s", orig.Role, wantJSON, gotJSON)
		}
	}
}

// TestProjectionSliceRoundTrip runs the whole conversation through the slice
// helpers and confirms wire-equality end to end.
func TestProjectionSliceRoundTrip(t *testing.T) {
	orig := flatMessages()
	got := ToMessages(ToSessionMessages(orig))
	if len(got) != len(orig) {
		t.Fatalf("length changed: want %d got %d", len(orig), len(got))
	}
	for i := range orig {
		w, _ := json.Marshal(orig[i])
		g, _ := json.Marshal(got[i])
		if string(w) != string(g) {
			t.Errorf("msg %d mismatch:\n want %s\n got  %s", i, w, g)
		}
	}
}

// TestProjectionRichToFlatDropsInterleaving documents the one lossy direction:
// interleaved-block ordering across block types collapses when projecting a rich
// message down to the flat shape.
func TestProjectionRichToFlatDropsInterleaving(t *testing.T) {
	rich := SessionMessage{Role: "assistant", Blocks: []Block{
		TextBlock{Text: "before "},
		ToolUseBlock{ID: "c1", Name: "t", Input: json.RawMessage(`{}`)},
		TextBlock{Text: "after"},
	}}
	flat := rich.ToMessage()
	if flat.Content != "before after" {
		t.Fatalf("expected text segments concatenated, got %q", flat.Content)
	}
	if len(flat.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(flat.ToolCalls))
	}
	// Re-projecting up no longer recovers the original interleaving: the tool
	// use now trails the text rather than sitting between the two segments.
	back := ToSessionMessage(flat)
	if len(back.Blocks) != 2 {
		t.Fatalf("expected 2 blocks (merged text + tool use), got %d", len(back.Blocks))
	}
	if _, ok := back.Blocks[0].(TextBlock); !ok {
		t.Fatalf("expected first block to be text, got %#v", back.Blocks[0])
	}
	if _, ok := back.Blocks[1].(ToolUseBlock); !ok {
		t.Fatalf("expected second block to be tool use, got %#v", back.Blocks[1])
	}
}

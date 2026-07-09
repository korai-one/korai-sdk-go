package korai

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMultimodalStringContentUnchanged verifies a plain string message still
// marshals to the original `"content":"..."` shape (backward compatibility).
func TestMultimodalStringContentUnchanged(t *testing.T) {
	m := Message{Role: "system", Content: "you are helpful"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"content":"you are helpful"`) {
		t.Errorf("string content not preserved: %s", got)
	}
	if strings.Contains(got, "[") {
		t.Errorf("string content should not be an array: %s", got)
	}
}

// TestMultimodalPartsMarshal verifies a parts message emits the OpenAI content
// array with text and image_url elements.
func TestMultimodalPartsMarshal(t *testing.T) {
	m := UserMessageWithParts(
		TextPart("what is this?"),
		ImagePart("data:image/png;base64,AAAA"),
	)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"content":[`,
		`"type":"text"`,
		`"text":"what is this?"`,
		`"type":"image_url"`,
		`"url":"data:image/png;base64,AAAA"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshalled parts missing %q:\n%s", want, got)
		}
	}
}

// TestMultimodalUnmarshalString accepts a plain-string content.
func TestMultimodalUnmarshalString(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hello"}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Content != "hello" || len(m.Parts) != 0 {
		t.Errorf("got Content=%q Parts=%v", m.Content, m.Parts)
	}
}

// TestMultimodalUnmarshalParts accepts an array content, populating Parts and
// flattening text into Content.
func TestMultimodalUnmarshalParts(t *testing.T) {
	raw := `{"role":"user","content":[{"type":"text","text":"caption: "},{"type":"image_url","image_url":{"url":"https://x/y.png"}},{"type":"text","text":"thanks"}]}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.Parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(m.Parts))
	}
	if m.Parts[1].Type != "image_url" || m.Parts[1].ImageURL == nil || m.Parts[1].ImageURL.URL != "https://x/y.png" {
		t.Errorf("image part not parsed: %+v", m.Parts[1])
	}
	if m.Content != "caption: thanks" {
		t.Errorf("text not flattened into Content, got %q", m.Content)
	}
}

// TestMultimodalRoundTrip marshals a parts message and unmarshals it back.
func TestMultimodalRoundTrip(t *testing.T) {
	orig := UserMessageWithParts(TextPart("hi"), ImagePart("data:image/jpeg;base64,ZZ"))
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Message
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Parts) != 2 || back.Parts[0].Text != "hi" || back.Parts[1].ImageURL.URL != "data:image/jpeg;base64,ZZ" {
		t.Errorf("round-trip mismatch: %+v", back.Parts)
	}
}

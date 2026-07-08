package korai

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// This file defines the rich, block-based canonical session type used for
// session STORAGE and cross-tool TELEPORT — not for inference. The inference
// wire stays the flat OpenAI-shaped Message (see llm.go); OpenAI compatibility
// is a product feature and is unchanged by anything here. Storage != inference.
//
// The canonical type is the SUPERSET of every producer's model: it carries
// ordered, tagged content blocks (text / tool-use / tool-result / image) so the
// richest producer (korai-code-cli's apiclient.Message + ContentBlock) persists
// without losing fidelity, while flat producers (cmd/kode, the dashboard) map
// UP into it trivially. See docs/HISTORY_SYNC.md §14 in the korai repo.

// SessionMessage is one turn in a stored conversation. Unlike the flat wire
// Message, its content is an ordered list of typed Blocks, so a single turn can
// interleave text, tool calls, tool results and images without loss. Role is one
// of "system" / "user" / "assistant" / "tool".
type SessionMessage struct {
	Role   string
	Blocks []Block
}

// Block is a sealed interface over the content variants that can appear in a
// SessionMessage. Use a type switch to inspect the concrete type. The set is
// TextBlock / ToolUseBlock / ToolResultBlock / ImageBlock; it is designed to
// cover korai-code-cli's full ContentBlock set so teleport is lossless.
type Block interface{ block() }

// TextBlock holds a plain-text content segment.
type TextBlock struct {
	Text string
}

// ToolUseBlock holds a tool invocation produced by the model. Input is the raw
// JSON argument object exactly as the model emitted it (kept compact so it
// round-trips byte-for-byte).
type ToolUseBlock struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResultBlock carries the result of an executed tool call back to the model.
// Name is the producing tool's name; it is optional and preserved so a flat
// role="tool" message (which carries a Name) survives a round trip.
type ToolResultBlock struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
}

// ImageBlock holds an image for a vision-capable model. Source is a data URI
// ("data:image/png;base64,<...>") or an https URL.
type ImageBlock struct {
	Source string
}

func (TextBlock) block()       {}
func (ToolUseBlock) block()    {}
func (ToolResultBlock) block() {}
func (ImageBlock) block()      {}

// Session is one stored conversation: metadata plus its ordered messages. It is
// the unit the sync client ships and the session store persists. Tool records
// which surface produced it (e.g. "korai-code-cli", "kode", "dashboard"); it is
// advisory metadata, not used for routing. Updated is derived from storage
// (file mtime / row column), not from content, so it never perturbs a content
// hash.
type Session struct {
	ID       string           `json:"id"`
	Created  time.Time        `json:"created"`
	Updated  time.Time        `json:"updated"`
	CWD      string           `json:"cwd"`
	Model    string           `json:"model"`
	Tool     string           `json:"tool,omitempty"`
	Messages []SessionMessage `json:"messages"`
}

// Block kind tags used in the JSON round-trip. The Block interface does not
// survive plain encoding/json, so SessionMessage marshals through a tagged DTO.
const (
	blockKindText       = "text"
	blockKindToolUse    = "tool_use"
	blockKindToolResult = "tool_result"
	blockKindImage      = "image"
)

// blockDTO is the tagged wire form of a Block. Only the fields relevant to the
// block's Kind are populated; the rest are omitted.
type blockDTO struct {
	Kind       string          `json:"kind"`
	Text       string          `json:"text,omitempty"`
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Content    string          `json:"content,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Source     string          `json:"source,omitempty"`
}

func blockToDTO(b Block) blockDTO {
	switch v := b.(type) {
	case TextBlock:
		return blockDTO{Kind: blockKindText, Text: v.Text}
	case ToolUseBlock:
		return blockDTO{Kind: blockKindToolUse, ID: v.ID, Name: v.Name, Input: v.Input}
	case ToolResultBlock:
		return blockDTO{Kind: blockKindToolResult, ToolCallID: v.ToolCallID, Name: v.Name, Content: v.Content, IsError: v.IsError}
	case ImageBlock:
		return blockDTO{Kind: blockKindImage, Source: v.Source}
	default:
		return blockDTO{Kind: blockKindText}
	}
}

func blockFromDTO(d blockDTO) Block {
	switch d.Kind {
	case blockKindText:
		return TextBlock{Text: d.Text}
	case blockKindToolUse:
		return ToolUseBlock{ID: d.ID, Name: d.Name, Input: compactRawJSON(d.Input)}
	case blockKindToolResult:
		return ToolResultBlock{ToolCallID: d.ToolCallID, Name: d.Name, Content: d.Content, IsError: d.IsError}
	case blockKindImage:
		return ImageBlock{Source: d.Source}
	default:
		return nil
	}
}

// sessionMessageDTO is the wire form of a SessionMessage: a role plus tagged
// blocks.
type sessionMessageDTO struct {
	Role   string     `json:"role"`
	Blocks []blockDTO `json:"blocks"`
}

// MarshalJSON renders a SessionMessage through the tagged block DTO so the Block
// interface round-trips.
func (m SessionMessage) MarshalJSON() ([]byte, error) {
	dto := sessionMessageDTO{Role: m.Role, Blocks: make([]blockDTO, 0, len(m.Blocks))}
	for _, b := range m.Blocks {
		dto.Blocks = append(dto.Blocks, blockToDTO(b))
	}
	return json.Marshal(dto)
}

// UnmarshalJSON reverses MarshalJSON, reconstructing concrete Block values from
// their Kind tags. Unknown block kinds are skipped rather than failing the decode
// so a newer producer's blocks don't break an older reader.
func (m *SessionMessage) UnmarshalJSON(data []byte) error {
	var dto sessionMessageDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return err
	}
	m.Role = dto.Role
	m.Blocks = make([]Block, 0, len(dto.Blocks))
	for _, d := range dto.Blocks {
		if b := blockFromDTO(d); b != nil {
			m.Blocks = append(m.Blocks, b)
		}
	}
	return nil
}

// compactRawJSON removes insignificant whitespace from raw. Empty or invalid
// input is returned unchanged. Keeps tool-call input compact so it round-trips
// byte-for-byte with what the model produced.
func compactRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return json.RawMessage(buf.Bytes())
}

// ---------------------------------------------------------------------------
// Rich <-> flat projection (§B).
//
// The flat Message (llm.go) is the OpenAI-shaped inference wire type. These
// helpers map between it and the canonical SessionMessage. flat -> rich -> flat
// is LOSSLESS for flat inputs. rich -> flat is lossy in one documented way:
// interleaved-block ORDERING across block *types* within a single message is
// not representable in the flat shape (all text collapses into Content/Parts,
// all tool-uses into ToolCalls), so a rich message that interleaves e.g.
// text/tool-use/text loses the relative position of the tool-use. IsError on a
// ToolResultBlock and any tool-result block past the first are also dropped
// (the flat shape carries neither).
// ---------------------------------------------------------------------------

// ToSessionMessage maps a flat wire Message UP into the canonical
// SessionMessage. A role="tool" message becomes a single ToolResultBlock;
// otherwise multimodal Parts (or the plain Content string) become text/image
// blocks, followed by one ToolUseBlock per tool call.
func ToSessionMessage(m Message) SessionMessage {
	if m.Role == "tool" {
		return SessionMessage{
			Role:   m.Role,
			Blocks: []Block{ToolResultBlock{ToolCallID: m.ToolCallID, Name: m.Name, Content: m.Content}},
		}
	}
	blocks := make([]Block, 0, len(m.Parts)+len(m.ToolCalls)+1)
	switch {
	case len(m.Parts) > 0:
		for _, p := range m.Parts {
			switch p.Type {
			case "image_url":
				if p.ImageURL != nil {
					blocks = append(blocks, ImageBlock{Source: p.ImageURL.URL})
				}
			default:
				blocks = append(blocks, TextBlock{Text: p.Text})
			}
		}
	case m.Content != "":
		blocks = append(blocks, TextBlock{Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, ToolUseBlock{ID: tc.ID, Name: tc.Name, Input: marshalToolInput(tc.Input)})
	}
	return SessionMessage{Role: m.Role, Blocks: blocks}
}

// ToMessage projects a canonical SessionMessage DOWN into a flat wire Message
// for inference. See the block comment above for what this direction drops.
func (m SessionMessage) ToMessage() Message {
	// A message bearing tool results maps to a flat role="tool" result.
	for _, b := range m.Blocks {
		if tr, ok := b.(ToolResultBlock); ok {
			return Message{Role: "tool", Content: tr.Content, Name: tr.Name, ToolCallID: tr.ToolCallID}
		}
	}

	out := Message{Role: m.Role}
	var texts []string
	var parts []ContentPart
	hasImage := false
	for _, b := range m.Blocks {
		switch v := b.(type) {
		case TextBlock:
			texts = append(texts, v.Text)
			parts = append(parts, TextPart(v.Text))
		case ImageBlock:
			hasImage = true
			parts = append(parts, ImagePart(v.Source))
		case ToolUseBlock:
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: v.ID, Name: v.Name, Input: decodeToolInput(v.Input)})
		}
	}
	if hasImage {
		out.Parts = parts
		// Mirror Message.UnmarshalJSON: the flattened text is also surfaced in
		// Content so string-only readers see it and struct round-trips match.
		out.Content = strings.Join(texts, "")
	} else {
		out.Content = strings.Join(texts, "")
	}
	return out
}

// ToSessionMessages / ToMessages are the slice forms of the projection.
func ToSessionMessages(msgs []Message) []SessionMessage {
	out := make([]SessionMessage, len(msgs))
	for i, m := range msgs {
		out[i] = ToSessionMessage(m)
	}
	return out
}

// ToMessages projects a canonical message slice down to the flat wire form.
func ToMessages(msgs []SessionMessage) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = m.ToMessage()
	}
	return out
}

// marshalToolInput encodes a decoded tool-call argument map back to compact raw
// JSON for storage. A nil map yields nil (no input).
func marshalToolInput(input map[string]any) json.RawMessage {
	if input == nil {
		return nil
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	return json.RawMessage(data)
}

// decodeToolInput parses a stored raw tool-call argument object into the flat
// ToolCall's map form. Anything that isn't a JSON object degrades to nil,
// matching decodeToolArgs in llm.go.
func decodeToolInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// assert the interface is satisfied at compile time.
var (
	_ Block = TextBlock{}
	_ Block = ToolUseBlock{}
	_ Block = ToolResultBlock{}
	_ Block = ImageBlock{}
)

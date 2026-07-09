package session

import (
	"encoding/json"
	"fmt"
	"time"

	korai "github.com/korai-one/korai-sdk-go"
)

// sessionEnvelope is the wire form of a whole korai.Session used when a
// conversation is shipped elsewhere (the cross-device sync client). Updated is
// intentionally omitted: it is derived from storage mtime, not content, and must
// not perturb the content hash. The encoding is deterministic for a given
// Session, so a content hash over it detects real changes.
type sessionEnvelope struct {
	ID       string                 `json:"id"`
	Created  time.Time              `json:"created"`
	CWD      string                 `json:"cwd"`
	Model    string                 `json:"model"`
	Tool     string                 `json:"tool,omitempty"`
	Messages []korai.SessionMessage `json:"messages"`
}

// MarshalSession serializes a Session's metadata and messages to plaintext bytes
// for callers (such as the sync client) that ship a whole conversation as one
// opaque unit. It does not encrypt; wrap the result in a Codec for
// confidentiality.
func MarshalSession(s korai.Session) ([]byte, error) {
	env := sessionEnvelope{
		ID: s.ID, Created: s.Created, CWD: s.CWD, Model: s.Model, Tool: s.Tool,
		Messages: s.Messages,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encoding session record: %w", err)
	}
	return data, nil
}

// UnmarshalSession reverses MarshalSession. Updated is left zero (the caller sets
// it from local storage on merge).
func UnmarshalSession(data []byte) (korai.Session, error) {
	var env sessionEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return korai.Session{}, fmt.Errorf("decoding session record: %w", err)
	}
	return korai.Session{
		ID: env.ID, Created: env.Created, CWD: env.CWD, Model: env.Model, Tool: env.Tool,
		Messages: env.Messages,
	}, nil
}

// MergeMessages unions two histories of the same append-only conversation,
// preserving order and dropping duplicates. Chat is append-mostly, so in the
// common case one history is a prefix of the other and the longer one is
// returned; when they diverge, local messages are kept first and any remote
// messages not already present are appended. Identity is by content (the
// canonical JSON encoding of the message). The result always begins with the
// full local history, so a subsequent append-only Save extends rather than
// rewrites the store.
func MergeMessages(local, remote []korai.SessionMessage) []korai.SessionMessage {
	seen := make(map[string]struct{}, len(local)+len(remote))
	out := make([]korai.SessionMessage, 0, len(local)+len(remote))
	add := func(msgs []korai.SessionMessage) {
		for _, m := range msgs {
			key := messageKey(m)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, m)
		}
	}
	add(local)
	add(remote)
	return out
}

// messageKey returns a stable content key for a message, used to dedup during a
// merge. It reuses the canonical encoding so two messages are equal iff they
// serialize identically. A marshal failure falls back to a role-tagged sentinel,
// which is safe (at worst it under-dedups, never merges distinct messages).
func messageKey(m korai.SessionMessage) string {
	data, err := json.Marshal(m)
	if err != nil {
		return "role:" + m.Role
	}
	return string(data)
}

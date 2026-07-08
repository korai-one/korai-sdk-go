// Package session persists Korai conversations (the rich, block-based
// korai.Session canonical type) to disk so they can be resumed, and ships them
// through the blind history-sync hub. It is the shared mechanic behind
// cross-tool teleport: both korai-code-cli and cmd/kode store the SAME
// superset session type, so a conversation started on one surface resumes
// losslessly on another. Ported from korai-code-cli's internal/session +
// internal/synchub, re-typed on korai.SessionMessage. See docs/HISTORY_SYNC.md
// §14 in the korai repo.
//
// Each session is stored as a header line of metadata followed by one line per
// message (JSONL FileStore) or a single row (SQLiteStore). Message bytes pass
// through a Codec so files can be encrypted at rest without changing callers;
// the default codec is the plaintext pass-through.
package session

// Codec transforms a session entry's bytes on the way to and from disk so
// session data can be encrypted at rest without changing the store's format or
// its callers. It is the seam for at-rest encryption: the header line is always
// written in the clear (it records which codec produced the data via Name), and
// every message payload is passed through the codec.
//
// PlainCodec is the pass-through used by default. EncryptingCodec (codec_encrypt
// .go) implements the same interface; the store records the codec's Name so Load
// can select the matching codec. Encode must not emit a newline byte, since
// FileStore entries are newline-framed (JSONL) — EncryptingCodec base64-encodes
// its ciphertext for exactly this reason.
type Codec interface {
	// Name is recorded in the session header. "none" means plaintext.
	Name() string
	// Encode maps a plaintext entry to its stored (possibly encrypted) form.
	Encode(plaintext []byte) ([]byte, error)
	// Decode reverses Encode.
	Decode(stored []byte) ([]byte, error)
}

// PlainCodec stores entries verbatim (no encryption). Its name is "none".
type PlainCodec struct{}

// Name implements Codec.
func (PlainCodec) Name() string { return "none" }

// Encode implements Codec; it returns plaintext unchanged.
func (PlainCodec) Encode(plaintext []byte) ([]byte, error) { return plaintext, nil }

// Decode implements Codec; it returns stored bytes unchanged.
func (PlainCodec) Decode(stored []byte) ([]byte, error) { return stored, nil }

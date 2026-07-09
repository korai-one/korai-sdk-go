package session_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	korai "github.com/korai-one/korai-sdk-go"
	"github.com/korai-one/korai-sdk-go/session"
)

func sampleSession(id string) korai.Session {
	return korai.Session{
		ID:      id,
		Created: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC),
		Updated: time.Date(2026, 7, 8, 10, 1, 0, 0, time.UTC),
		CWD:     "/w",
		Model:   "auto",
		Tool:    "kode",
		Messages: []korai.SessionMessage{
			{Role: "user", Blocks: []korai.Block{korai.TextBlock{Text: "hi"}}},
			{Role: "assistant", Blocks: []korai.Block{
				korai.TextBlock{Text: "ok"},
				korai.ToolUseBlock{ID: "c1", Name: "grep", Input: json.RawMessage(`{"p":"x"}`)},
			}},
			{Role: "tool", Blocks: []korai.Block{
				korai.ToolResultBlock{ToolCallID: "c1", Content: "found"},
			}},
		},
	}
}

// messagesEqual compares message slices ignoring the Updated field, which stores
// derive from mtime rather than content.
func messagesEqual(t *testing.T, want, got []korai.SessionMessage) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("messages mismatch:\n want %#v\n got  %#v", want, got)
	}
}

func TestFileStoreSaveLoadPlaintext(t *testing.T) {
	dir := t.TempDir()
	st := session.NewFileStore(dir)
	sess := sampleSession("s1")
	if err := st.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load("s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	messagesEqual(t, sess.Messages, got.Messages)
	if got.Tool != "kode" || got.Model != "auto" || got.CWD != "/w" {
		t.Fatalf("metadata mismatch: %+v", got)
	}
}

func TestFileStoreAppend(t *testing.T) {
	dir := t.TempDir()
	st := session.NewFileStore(dir)
	sess := sampleSession("s1")
	if err := st.Save(sess); err != nil {
		t.Fatalf("save1: %v", err)
	}
	sess.Messages = append(sess.Messages, korai.SessionMessage{
		Role: "user", Blocks: []korai.Block{korai.TextBlock{Text: "more"}},
	})
	if err := st.Save(sess); err != nil {
		t.Fatalf("save2: %v", err)
	}
	got, err := st.Load("s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("want 4 messages after append, got %d", len(got.Messages))
	}
	messagesEqual(t, sess.Messages, got.Messages)
}

func TestFileStoreEncryptedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	codec, err := session.NewEncryptingCodec(key)
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	st := session.NewFileStore(dir).WithCodec(codec)
	sess := sampleSession("enc1")
	if err := st.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load("enc1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	messagesEqual(t, sess.Messages, got.Messages)

	// A store without the key cannot decode the message lines.
	if _, err := session.NewFileStore(dir).Load("enc1"); err == nil {
		t.Fatal("expected load without codec to fail")
	}
}

func TestFileStoreListAndDelete(t *testing.T) {
	dir := t.TempDir()
	st := session.NewFileStore(dir)
	if err := st.Save(sampleSession("a")); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(sampleSession("b")); err != nil {
		t.Fatal(err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
	if err := st.Delete("a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.Delete("a"); err != nil {
		t.Fatalf("delete idempotent: %v", err)
	}
	if _, err := st.Load("a"); err == nil {
		t.Fatal("expected load of deleted session to fail")
	}
}

func TestSQLiteStoreSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := session.NewSQLiteStore(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	sess := sampleSession("s1")
	if err := st.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Load("s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	messagesEqual(t, sess.Messages, got.Messages)
	if !got.Created.Equal(sess.Created) {
		t.Fatalf("created mismatch: want %v got %v", sess.Created, got.Created)
	}

	latest, ok, err := st.Latest("/w")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if latest.ID != "s1" {
		t.Fatalf("latest id = %q", latest.ID)
	}
}

func TestEncryptingCodecTamperFails(t *testing.T) {
	key := make([]byte, 32)
	codec, err := session.NewEncryptingCodec(key)
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	enc, err := codec.Encode([]byte("secret"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec, err := codec.Decode(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(dec) != "secret" {
		t.Fatalf("roundtrip mismatch: %q", dec)
	}
	enc[len(enc)-2] ^= 0xff // flip a byte inside the base64 payload
	if _, err := codec.Decode(enc); err == nil {
		t.Fatal("expected tamper to fail AEAD auth")
	}
}

func TestEncryptingCodecWrongKeyLen(t *testing.T) {
	if _, err := session.NewEncryptingCodec(make([]byte, 16)); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}

func TestMarshalUnmarshalSession(t *testing.T) {
	sess := sampleSession("s1")
	data, err := session.MarshalSession(sess)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := session.UnmarshalSession(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Updated is intentionally not carried in the envelope.
	if !got.Updated.IsZero() {
		t.Fatalf("expected zero Updated, got %v", got.Updated)
	}
	messagesEqual(t, sess.Messages, got.Messages)
	if got.ID != sess.ID || got.Tool != sess.Tool {
		t.Fatalf("metadata mismatch: %+v", got)
	}
}

func TestMergeMessagesUnion(t *testing.T) {
	local := []korai.SessionMessage{
		{Role: "user", Blocks: []korai.Block{korai.TextBlock{Text: "a"}}},
		{Role: "assistant", Blocks: []korai.Block{korai.TextBlock{Text: "b"}}},
	}
	remote := []korai.SessionMessage{
		{Role: "user", Blocks: []korai.Block{korai.TextBlock{Text: "a"}}}, // dup
		{Role: "assistant", Blocks: []korai.Block{korai.TextBlock{Text: "b"}}},
		{Role: "user", Blocks: []korai.Block{korai.TextBlock{Text: "c"}}}, // new
	}
	merged := session.MergeMessages(local, remote)
	if len(merged) != 3 {
		t.Fatalf("want 3 merged, got %d", len(merged))
	}
	last := merged[2].Blocks[0].(korai.TextBlock)
	if last.Text != "c" {
		t.Fatalf("expected appended 'c', got %q", last.Text)
	}
}

func TestDeriveSyncIDStable(t *testing.T) {
	key := make([]byte, 32)
	a := session.DeriveSyncID(key)
	b := session.DeriveSyncID(key)
	if a == "" || a != b {
		t.Fatalf("sync id not stable: %q vs %q", a, b)
	}
	key[0] = 1
	if session.DeriveSyncID(key) == a {
		t.Fatal("different key should derive a different sync id")
	}
}

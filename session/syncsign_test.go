package session_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"github.com/korai-one/korai-sdk-go/session"
)

// testFolderKey is a deterministic 32-byte K_folder for the signing tests.
func testFolderKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

// TestDeriveWritePublicKeyDeterministic checks the write pubkey is a stable,
// unpadded-base64url-encoded 32-byte Ed25519 key: same key in, same string out,
// and a different key yields a different pubkey.
func TestDeriveWritePublicKeyDeterministic(t *testing.T) {
	key := testFolderKey()
	got := session.DeriveWritePublicKey(key)
	if got != session.DeriveWritePublicKey(key) {
		t.Fatal("DeriveWritePublicKey is not deterministic")
	}
	pub, err := base64.RawURLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("pubkey is not unpadded base64url: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("pubkey length = %d, want %d", len(pub), ed25519.PublicKeySize)
	}

	other := make([]byte, 32)
	copy(other, key)
	other[0] ^= 0xff
	if session.DeriveWritePublicKey(other) == got {
		t.Fatal("different keys produced the same write pubkey")
	}
}

// TestWritePubkeyCrossLangVector pins the write pubkey for the shared fixed key
// (bytes 0x00..0x1f) to the exact value the browser derives (see
// VECTOR_WRITE_PUBKEY in packages/chat-ui/src/sync-crypto.test.ts). Any drift in
// the HKDF params or Ed25519 seeding between the two stacks would make the hub's
// TOFU pin reject a device signing with the other stack's key.
func TestWritePubkeyCrossLangVector(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	const want = "9IuM9xlE2tOKiWEou2Jvz7OuhN9RnFZfk-33sy8PlpI"
	if got := session.DeriveWritePublicKey(key); got != want {
		t.Errorf("write pubkey drift: got %q, want %q (must match browser deriveWritePublicKey)", got, want)
	}
}

// TestSignSyncRequestVerifies confirms a signature produced by SignSyncRequest
// verifies against the derived public key with stdlib ed25519.Verify, and that a
// tampered payload or a wrong key fails verification.
func TestSignSyncRequestVerifies(t *testing.T) {
	key := testFolderKey()
	payload := session.SyncCanonicalPayload("PUT", "sync123", "item42", "hashcafe")

	sigStr := session.SignSyncRequest(key, payload)
	sig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil {
		t.Fatalf("signature is not unpadded base64url: %v", err)
	}
	pub, err := base64.RawURLEncoding.DecodeString(session.DeriveWritePublicKey(key))
	if err != nil {
		t.Fatalf("decoding pubkey: %v", err)
	}
	if !ed25519.Verify(pub, []byte(payload), sig) {
		t.Fatal("signature did not verify against derived pubkey")
	}
	if ed25519.Verify(pub, []byte(payload+"x"), sig) {
		t.Fatal("signature verified for a tampered payload")
	}

	other := make([]byte, 32)
	copy(other, key)
	other[31] ^= 0x01
	otherPub, _ := base64.RawURLEncoding.DecodeString(session.DeriveWritePublicKey(other))
	if ed25519.Verify(otherPub, []byte(payload), sig) {
		t.Fatal("signature verified under the wrong key's pubkey")
	}
}

// TestSyncCanonicalPayload pins the exact byte layout of the three mutating-op
// payloads. These strings are a frozen wire contract shared with the hub and the
// browser; any change breaks cross-device signature verification.
func TestSyncCanonicalPayload(t *testing.T) {
	cases := []struct {
		name      string
		op        string
		syncID    string
		itemID    string
		blockHash string
		want      string
	}{
		{
			name: "PUT", op: "PUT", syncID: "S", itemID: "I", blockHash: "H",
			want: "korai-sync-v1\nPUT\nS\nI\nH",
		},
		{
			name: "TOMBSTONE", op: "TOMBSTONE", syncID: "S", itemID: "I",
			want: "korai-sync-v1\nTOMBSTONE\nS\nI",
		},
		{
			name: "DELETE", op: "DELETE", syncID: "S",
			want: "korai-sync-v1\nDELETE\nS",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := session.SyncCanonicalPayload(tc.op, tc.syncID, tc.itemID, tc.blockHash)
			if got != tc.want {
				t.Fatalf("payload = %q, want %q", got, tc.want)
			}
		})
	}
}

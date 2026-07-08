package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// EncryptingCodecName is the codec name recorded in a session header/row when
// entries are encrypted with EncryptingCodec. It is versioned so a future
// algorithm change (e.g. XChaCha20-Poly1305) can coexist and be selected by
// name on Load.
const EncryptingCodecName = "aes256gcm-folder-v1"

// contentKeyLen is the required length of the content key K_folder.
const contentKeyLen = 32

// syncIDInfo is the HMAC domain-separation label for the namespace-bearer
// derivation. It matches korai-code-cli's synckey package so a device using
// either surface derives the SAME sync_id from the same key. Frozen.
const syncIDInfo = "korai-sync-id"

// ErrKeyLength is returned when a content key is not exactly 32 bytes.
var ErrKeyLength = errors.New("session content key must be 32 bytes")

// EncryptingCodec encrypts each session entry with AES-256-GCM under a 32-byte
// content key (K_folder). A fresh random nonce is generated per entry and
// prepended to the ciphertext; the whole (nonce||ciphertext) is base64-encoded
// so the result is newline-free and safe for the JSONL framing the store uses.
// Decode reverses this; a wrong key or a tampered entry fails the AEAD
// authentication check and returns an error.
//
// AES-256-GCM (stdlib crypto/aes + crypto/cipher) is used so the codec adds no
// new dependency; the versioned Name lets an XChaCha codec drop in later without
// breaking existing files.
//
// Key source: the caller supplies the raw 32-byte key, loaded from
// KORAI_SYNC_KEY or ~/.korai/sync.key via LoadContentKey. The Codec is agnostic
// to how the key was obtained (the cross-device key distribution — BIP39
// mnemonic, terminal QR, passphrase recovery — lives outside this package).
type EncryptingCodec struct {
	aead cipher.AEAD
}

// NewEncryptingCodec returns a codec that encrypts entries under key, which must
// be exactly 32 bytes (a 256-bit AES key). It returns ErrKeyLength otherwise.
func NewEncryptingCodec(key []byte) (*EncryptingCodec, error) {
	if len(key) != contentKeyLen {
		return nil, fmt.Errorf("%w: got %d", ErrKeyLength, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initializing cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initializing gcm: %w", err)
	}
	return &EncryptingCodec{aead: aead}, nil
}

// Name implements Codec; it returns EncryptingCodecName.
func (*EncryptingCodec) Name() string { return EncryptingCodecName }

// Encode implements Codec. It seals plaintext with a fresh random nonce and
// returns base64(nonce||ciphertext). The output never contains a newline, so it
// is safe as a single JSONL entry line.
func (c *EncryptingCodec) Encode(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	sealed := c.aead.Seal(nil, nonce, plaintext, nil)
	blob := make([]byte, 0, len(nonce)+len(sealed))
	blob = append(blob, nonce...)
	blob = append(blob, sealed...)
	out := make([]byte, base64.StdEncoding.EncodedLen(len(blob)))
	base64.StdEncoding.Encode(out, blob)
	return out, nil
}

// Decode implements Codec. It reverses Encode; a wrong key or a tampered entry
// fails GCM authentication and returns an error.
func (c *EncryptingCodec) Decode(stored []byte) ([]byte, error) {
	blob := make([]byte, base64.StdEncoding.DecodedLen(len(stored)))
	n, err := base64.StdEncoding.Decode(blob, stored)
	if err != nil {
		return nil, fmt.Errorf("decoding session entry: %w", err)
	}
	blob = blob[:n]
	ns := c.aead.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("session entry too short")
	}
	nonce, sealed := blob[:ns], blob[ns:]
	plain, err := c.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("authenticating session entry: %w", err)
	}
	return plain, nil
}

// LoadContentKey resolves the 32-byte content key K_folder from, in order of
// precedence, the KORAI_SYNC_KEY environment variable then the key file at
// <home>/.korai/sync.key. The value may be hex- or base64-encoded (standard or
// URL-safe); surrounding whitespace is ignored. It returns ok=false with a nil
// error when no key is configured (so callers treat "no key" as "encryption
// off"), and a non-nil error only when a key is present but malformed.
func LoadContentKey(home string) (key []byte, ok bool, err error) {
	if raw := strings.TrimSpace(os.Getenv("KORAI_SYNC_KEY")); raw != "" {
		k, derr := decodeKey(raw)
		if derr != nil {
			return nil, false, fmt.Errorf("KORAI_SYNC_KEY: %w", derr)
		}
		return k, true, nil
	}
	path := filepath.Join(home, ".korai", "sync.key")
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading %s: %w", path, rerr)
	}
	k, derr := decodeKey(strings.TrimSpace(string(data)))
	if derr != nil {
		return nil, false, fmt.Errorf("%s: %w", path, derr)
	}
	return k, true, nil
}

// decodeKey parses a 32-byte key from a hex or base64 string. It tries hex
// first (a 64-char hex string is unambiguous), then standard and URL-safe
// base64, and validates the decoded length.
func decodeKey(s string) ([]byte, error) {
	if k, err := hex.DecodeString(s); err == nil && len(k) == contentKeyLen {
		return k, nil
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if k, err := enc.DecodeString(s); err == nil && len(k) == contentKeyLen {
			return k, nil
		}
	}
	return nil, fmt.Errorf("%w (want 32 bytes hex or base64)", ErrKeyLength)
}

// DeriveSyncID returns the opaque namespace bearer for key:
// base64url(HMAC-SHA256(K_folder, "korai-sync-id")). It is deterministic, so
// every device holding the same key targets the same hub namespace, and it
// reveals nothing about K_folder (learning it does not grant decryption).
// Matches korai-code-cli's synckey.DeriveSyncID.
func DeriveSyncID(key []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(syncIDInfo))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// HashCiphertext returns the hex-encoded SHA-256 of the raw ciphertext bytes,
// the content-address used as a block hash by the sync client. It lives here so
// the hashing convention sits next to the codec that produces the ciphertext.
func HashCiphertext(ciphertext []byte) string {
	sum := sha256.Sum256(ciphertext)
	return hex.EncodeToString(sum[:])
}

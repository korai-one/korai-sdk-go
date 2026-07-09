// Package synchub is an opt-in, poll-based client for the blind history-sync
// hub. It ships each local conversation to the hub as one opaque, client-side
// encrypted block and pulls other devices' blocks back, merging them into the
// local session store. The hub stores only ciphertext addressed by an opaque
// namespace handle (sync_id); it never receives the content key, so it cannot
// read anything. See docs/HISTORY_SYNC.md §5, §11 in the korai repo.
//
// Sync is OFF by default. With no configuration the package makes zero network
// calls and has no effect: New returns a nil *Syncer whose methods are no-ops.
//
// Ported from korai-code-cli's internal/synchub, re-typed on the shared
// korai.Session canonical type via the sibling session package.
package synchub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/korai-one/korai-sdk-go/session"
)

// maxBlockBytes caps a single fetched ciphertext block to bound memory against a
// hostile or buggy hub. Whole-session blocks are small; 32 MiB is generous.
const maxBlockBytes = 32 << 20

// Client is the HTTP client for the blind sync hub. All requests carry the
// opaque namespace handle as a bearer token (Authorization: Bearer <sync_id>);
// the sync_id never appears in a URL path. The hub sees only ciphertext and
// opaque hashes.
type Client struct {
	base   string // e.g. https://hub.example/v1/sync
	syncID string
	key    []byte // K_folder, for deriving the Ed25519 write-signature (may be nil)
	http   *http.Client
}

// NewClient returns a Client rooted at baseURL (the KORAI_SYNC_URL origin; the
// "/v1/sync" prefix is appended here) authenticating as syncID. key is the
// 32-byte content key K_folder used to sign mutating requests (PUT, tombstone,
// namespace delete) with the derived Ed25519 write key; when key is empty those
// requests go out unsigned (used by low-level transport tests). A nil hc uses
// http.DefaultClient.
func NewClient(baseURL, syncID string, key []byte, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		base:   strings.TrimRight(baseURL, "/") + "/v1/sync",
		syncID: syncID,
		key:    key,
		http:   hc,
	}
}

// ManifestEntry is one item version advertised by the hub. Tombstone marks a
// delete; BlockHash is the content address of its ciphertext.
type ManifestEntry struct {
	ItemID    string `json:"item_id"`
	BlockHash string `json:"block_hash"`
	Seq       int64  `json:"seq"`
	ByteSize  int64  `json:"byte_size"`
	Tombstone bool   `json:"tombstone"`
}

// Manifest is a page of changed items since a cursor, plus the next cursor. It
// also carries the namespace-wide nuke marker (attack model T11): when Nuked is
// true the hub is asserting the whole namespace was ordered destroyed, and
// NukeSig/NukePubkey are the holder-of-K_folder proof a client MUST verify
// before acting (see Syncer.verifyNuke) so a malicious hub cannot force a wipe.
type Manifest struct {
	Entries    []ManifestEntry `json:"entries"`
	Next       int64           `json:"next"`
	Nuked      bool            `json:"nuked"`
	NukeSig    string          `json:"nuke_sig"`    // base64url Ed25519 sig over the NUKE marker
	NukePubkey string          `json:"nuke_pubkey"` // base64url write public key that signed it
}

// putBlockRequest is the body of PUT /v1/sync/blocks.
type putBlockRequest struct {
	ItemID     string `json:"item_id"`
	BlockHash  string `json:"block_hash"`
	Ciphertext string `json:"ciphertext"` // base64
}

// seqResponse is the {seq} reply shared by writes.
type seqResponse struct {
	Seq int64 `json:"seq"`
}

// tombstoneRequest is the body of POST /v1/sync/tombstone.
type tombstoneRequest struct {
	ItemID string `json:"item_id"`
}

// PutBlock uploads one ciphertext block for itemID and returns its assigned
// sequence. blockHash must be hex(sha256(ciphertext)); the hub is idempotent by
// it, so re-uploading the same bytes is safe.
func (c *Client) PutBlock(ctx context.Context, itemID, blockHash string, ciphertext []byte) (int64, error) {
	body, err := json.Marshal(putBlockRequest{
		ItemID:     itemID,
		BlockHash:  blockHash,
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	})
	if err != nil {
		return 0, fmt.Errorf("encoding put-block: %w", err)
	}
	var out seqResponse
	payload := session.SyncCanonicalPayload("PUT", c.syncID, itemID, blockHash)
	if err := c.do(ctx, http.MethodPut, c.base+"/blocks", bytes.NewReader(body), &out, c.sigHeaders(payload)); err != nil {
		return 0, err
	}
	return out.Seq, nil
}

// Manifest fetches changed items with seq greater than since, up to limit
// entries (limit <= 0 omits the parameter and lets the hub choose).
func (c *Client) Manifest(ctx context.Context, since int64, limit int) (Manifest, error) {
	q := url.Values{}
	q.Set("since", strconv.FormatInt(since, 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out Manifest
	if err := c.do(ctx, http.MethodGet, c.base+"/manifest?"+q.Encode(), nil, &out, nil); err != nil {
		return Manifest{}, err
	}
	return out, nil
}

// GetBlock fetches the raw ciphertext bytes for a content hash.
func (c *Client) GetBlock(ctx context.Context, hash string) ([]byte, error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.base+"/blocks/"+url.PathEscape(hash), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching block %s: %w", hash, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError("get block", resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBlockBytes))
	if err != nil {
		return nil, fmt.Errorf("reading block %s: %w", hash, err)
	}
	return data, nil
}

// WipeRemote deletes the entire namespace on the hub (DELETE /v1/sync): all
// ciphertext blocks and tombstones for this sync_id. It is idempotent — a second
// call, or a call against an already-empty namespace, still succeeds. It backs
// the duress wipe's remote-purge step; callers treat a network failure as
// non-fatal because the local crypto-shred already makes the ciphertext
// unreadable.
func (c *Client) WipeRemote(ctx context.Context) error {
	payload := session.SyncCanonicalPayload("DELETE", c.syncID, "", "")
	return c.do(ctx, http.MethodDelete, c.base, nil, nil, c.sigHeaders(payload))
}

// nukeSigDomain is the version tag that prefixes every sync signing payload. It
// equals session.syncSigDomain (unexported there) and matches the hub and the
// browser byte-for-byte. The NUKE marker is built here rather than through
// session.SyncCanonicalPayload because that helper's op set ("PUT"/"TOMBSTONE"/
// "DELETE") is frozen and deliberately does not include NUKE.
const nukeSigDomain = "korai-sync-v1"

// nukePayload returns the canonical fleet-wide NUKE marker for syncID:
//
//	"korai-sync-v1\nNUKE\n<sync_id>"
//
// UTF-8, no trailing newline. It must match the hub and the browser exactly so
// a signature made on any surface verifies on every other.
func nukePayload(syncID string) string {
	return nukeSigDomain + "\nNUKE\n" + syncID
}

// PostNuke marks the entire namespace as fleet-wide nuked on the hub
// (POST /v1/sync/nuke). It signs the canonical NUKE marker with the derived
// Ed25519 write key and sends the standard X-Korai-Sync-Pubkey / X-Korai-Sync-Sig
// headers, so the hub — and every peer that later pulls the manifest — can prove
// the wipe was ordered by a holder of K_folder (attack model T11: a malicious
// hub cannot forge it). It is best-effort, mirroring WipeRemote: the CLI's
// `korai nuke` calls it before it crypto-shreds K_folder, and a network failure
// is non-fatal because the local shred already renders the ciphertext
// unreadable. When no content key is configured the request goes out unsigned
// (the hub will reject it), matching the other mutating calls.
func (c *Client) PostNuke(ctx context.Context) error {
	payload := nukePayload(c.syncID)
	return c.do(ctx, http.MethodPost, c.base+"/nuke", nil, nil, c.sigHeaders(payload))
}

// Tombstone records a delete for itemID and returns its sequence.
func (c *Client) Tombstone(ctx context.Context, itemID string) (int64, error) {
	body, err := json.Marshal(tombstoneRequest{ItemID: itemID})
	if err != nil {
		return 0, fmt.Errorf("encoding tombstone: %w", err)
	}
	var out seqResponse
	payload := session.SyncCanonicalPayload("TOMBSTONE", c.syncID, itemID, "")
	if err := c.do(ctx, http.MethodPost, c.base+"/tombstone", bytes.NewReader(body), &out, c.sigHeaders(payload)); err != nil {
		return 0, err
	}
	return out.Seq, nil
}

// sigHeaders returns the Ed25519 write-signature headers for payload, or nil
// when no content key is configured (unsigned transport, e.g. low-level tests).
// A signed mutating request proves the sender holds K_folder, closing the
// forge/replay gap (attack-model T5/T6) that a bearer-only sync_id left open.
func (c *Client) sigHeaders(payload string) map[string]string {
	if len(c.key) == 0 {
		return nil
	}
	return map[string]string{
		"X-Korai-Sync-Pubkey": session.DeriveWritePublicKey(c.key),
		"X-Korai-Sync-Sig":    session.SignSyncRequest(c.key, payload),
	}
}

// do performs a JSON request and decodes a JSON response into out (which may be
// nil to discard the body). extra headers (e.g. the write-signature pair) are
// attached after the bearer auth; pass nil for none.
func (c *Client) do(ctx context.Context, method, u string, body io.Reader, out any, extra map[string]string) error {
	req, err := c.newRequest(ctx, method, u, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(method+" "+u, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s response: %w", u, err)
	}
	return nil
}

// newRequest builds a request with the bearer auth header attached.
func (c *Client) newRequest(ctx context.Context, method, u string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.syncID)
	return req, nil
}

// statusError reads a bounded snippet of an error response body for context.
func statusError(what string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s: hub returned %d: %s", what, resp.StatusCode, bytes.TrimSpace(snippet))
}

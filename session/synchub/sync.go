package synchub

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	korai "github.com/korai-one/korai-sdk-go"
	"github.com/korai-one/korai-sdk-go/session"
)

// manifestLimit is the page size requested from the hub manifest.
const manifestLimit = 500

// ErrNamespaceNuked is returned by Sync/pull when the hub manifest carries a
// cryptographically valid fleet-wide nuke marker for this namespace (attack
// model T11). It is a terminal signal: a holder of K_folder ordered the whole
// history destroyed, so the caller should stop syncing — Run does so
// automatically — after any registered self-destruct callback has run.
var ErrNamespaceNuked = errors.New("synchub: namespace nuked")

// cursorFilePerm keeps the local sync cursor private to the user.
const cursorFilePerm os.FileMode = 0o600

// deleter is the optional capability a session store may implement to apply
// tombstones. Both FileStore and SQLiteStore satisfy it; if a store does not,
// tombstones are logged and skipped.
type deleter interface {
	Delete(id string) error
}

// Syncer performs one-shot and background push/pull against the blind hub. It is
// safe for a single goroutine plus the background ticker; its bookkeeping maps
// are mutex-guarded. A nil *Syncer is a valid disabled instance whose methods
// are no-ops, so callers need not branch on whether sync is configured.
type Syncer struct {
	cfg    Config
	client *Client
	store  session.Store
	codec  session.Codec
	log    *slog.Logger

	mu     sync.Mutex
	pushed map[string]string   // item_id -> plaintext content hash last synced
	have   map[string]struct{} // ciphertext block hashes we already possess

	onNuke func(context.Context) error // optional self-destruct hook, set via OnNuke
}

// New builds a Syncer from a resolved Config and the local session store. When
// cfg.Enabled is false it returns (nil, nil): sync is off and every method is a
// no-op. The transport encryption uses the same content key as the store's
// at-rest codec; logger defaults to slog.Default when nil.
func New(cfg Config, store session.Store, logger *slog.Logger) (*Syncer, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if store == nil {
		return nil, errors.New("synchub: nil session store")
	}
	codec, err := session.NewEncryptingCodec(cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("synchub: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{
		cfg:    cfg,
		client: NewClient(cfg.URL, cfg.SyncID, cfg.Key, nil),
		store:  store,
		codec:  codec,
		log:    logger,
		pushed: make(map[string]string),
		have:   make(map[string]struct{}),
	}, nil
}

// OnNuke registers a self-destruct callback invoked once — before
// ErrNamespaceNuked is returned — when a pull observes a cryptographically valid
// fleet-wide nuke marker for this namespace (attack model T11). The CLI wires it
// to crypto-shred K_folder and purge the local session store. It is optional:
// with no handler a verified nuke still surfaces as ErrNamespaceNuked for the
// caller to act on. A nil Syncer ignores the call.
func (s *Syncer) OnNuke(fn func(context.Context) error) {
	if s == nil {
		return
	}
	s.onNuke = fn
}

// Run drives Sync on a ticker until ctx is cancelled. Start it in a goroutine. A
// nil Syncer returns immediately. Tick failures are logged and retried on the
// next tick; they never stop the loop — except a verified fleet-wide nuke
// (ErrNamespaceNuked), which is terminal: the loop stops after the self-destruct
// hook has run.
func (s *Syncer) Run(ctx context.Context) {
	if s == nil {
		return
	}
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
		if errors.Is(err, ErrNamespaceNuked) {
			s.log.Warn("namespace nuked; stopping sync", "sync_id", s.cfg.SyncID)
			return
		}
		s.log.Warn("initial sync failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
				if errors.Is(err, ErrNamespaceNuked) {
					s.log.Warn("namespace nuked; stopping sync", "sync_id", s.cfg.SyncID)
					return
				}
				s.log.Warn("sync tick failed", "error", err)
			}
		}
	}
}

// Sync performs one push followed by one pull. A nil Syncer is a no-op. Both
// halves are attempted even if one reports errors; the combined error (if any)
// is returned for the caller to log.
func (s *Syncer) Sync(ctx context.Context) error {
	if s == nil {
		return nil
	}
	pushErr := s.push(ctx)
	pullErr := s.pull(ctx)
	return errors.Join(pushErr, pullErr)
}

// push uploads every local session whose plaintext changed since it was last
// synced, and tombstones sessions that have disappeared locally. Per-item errors
// are logged and the item is left un-marked so it retries next tick; push never
// aborts on the first failure.
func (s *Syncer) push(ctx context.Context) error {
	sessions, err := s.store.List()
	if err != nil {
		return fmt.Errorf("listing sessions for push: %w", err)
	}

	present := make(map[string]struct{}, len(sessions))
	var failures int
	for _, sess := range sessions {
		present[sess.ID] = struct{}{}
		plaintext, err := session.MarshalSession(sess)
		if err != nil {
			s.log.Warn("serializing session for push", "id", sess.ID, "error", err)
			failures++
			continue
		}
		h := contentHash(plaintext)

		s.mu.Lock()
		unchanged := s.pushed[sess.ID] == h
		s.mu.Unlock()
		if unchanged {
			continue
		}

		blob, err := s.codec.Encode(padToBucket(plaintext))
		if err != nil {
			s.log.Warn("encrypting session for push", "id", sess.ID, "error", err)
			failures++
			continue
		}
		blockHash := session.HashCiphertext(blob)
		if _, err := s.client.PutBlock(ctx, sess.ID, blockHash, blob); err != nil {
			s.log.Warn("pushing session block", "id", sess.ID, "error", err)
			failures++
			continue
		}
		s.mu.Lock()
		s.pushed[sess.ID] = h
		s.have[blockHash] = struct{}{} // our own block; no need to pull it back
		s.mu.Unlock()
	}

	failures += s.pushTombstones(ctx, present)
	if failures > 0 {
		return fmt.Errorf("push: %d item(s) failed", failures)
	}
	return nil
}

// pushTombstones tombstones every id we previously synced that is no longer
// present locally (a deletion). Detection is limited to ids synced in this
// process; cross-restart deletions are a documented v1 limitation.
func (s *Syncer) pushTombstones(ctx context.Context, present map[string]struct{}) int {
	s.mu.Lock()
	var gone []string
	for id := range s.pushed {
		if _, ok := present[id]; !ok {
			gone = append(gone, id)
		}
	}
	s.mu.Unlock()

	var failures int
	for _, id := range gone {
		if _, err := s.client.Tombstone(ctx, id); err != nil {
			s.log.Warn("tombstoning session", "id", id, "error", err)
			failures++
			continue
		}
		s.mu.Lock()
		delete(s.pushed, id)
		s.mu.Unlock()
	}
	return failures
}

// pull fetches manifest pages since the local cursor, merging remote blocks and
// applying tombstones. The cursor advances only after a page is fully processed,
// so a mid-page failure is retried next tick (merges are idempotent, so a replay
// is harmless).
func (s *Syncer) pull(ctx context.Context) error {
	cursor, err := s.loadCursor()
	if err != nil {
		return err
	}
	for {
		manifest, err := s.client.Manifest(ctx, cursor, manifestLimit)
		if err != nil {
			return fmt.Errorf("pulling manifest: %w", err)
		}
		if manifest.Nuked {
			if s.verifyNuke(manifest) {
				return s.handleNuke(ctx)
			}
			// A malicious or buggy hub must NOT be able to force a wipe: an
			// unverifiable marker is logged and ignored, and normal manifest
			// processing continues.
			s.log.Warn("ignoring namespace nuke marker with invalid signature", "sync_id", s.cfg.SyncID)
		}
		if len(manifest.Entries) == 0 {
			break
		}
		if perr := s.applyManifest(ctx, manifest.Entries); perr != nil {
			return perr
		}
		if manifest.Next <= cursor {
			break // no forward progress; avoid an infinite loop
		}
		cursor = manifest.Next
		if err := s.saveCursor(cursor); err != nil {
			return err
		}
	}
	return nil
}

// applyManifest processes one page. Entries are grouped per item; if an item's
// most recent action (highest seq) is a tombstone, the item is deleted.
// Otherwise every not-yet-seen block for the item is merged. Merging all blocks
// (not just the highest-seq one) is what makes the append-only union correct
// when peers push overlapping histories. Union is commutative, so order does not
// matter.
func (s *Syncer) applyManifest(ctx context.Context, entries []ManifestEntry) error {
	byItem := make(map[string][]ManifestEntry)
	var order []string
	for _, e := range entries {
		if _, seen := byItem[e.ItemID]; !seen {
			order = append(order, e.ItemID)
		}
		byItem[e.ItemID] = append(byItem[e.ItemID], e)
	}
	sort.Strings(order) // deterministic, test-stable iteration

	var failures int
	for _, id := range order {
		items := byItem[id]
		sort.Slice(items, func(i, j int) bool { return items[i].Seq < items[j].Seq })
		if items[len(items)-1].Tombstone {
			if err := s.applyTombstone(id); err != nil {
				s.log.Warn("applying tombstone", "id", id, "error", err)
				failures++
			}
			continue
		}
		for _, e := range items {
			if e.Tombstone {
				continue
			}
			if err := s.applyBlock(ctx, e); err != nil {
				s.log.Warn("applying remote block", "id", id, "hash", e.BlockHash, "error", err)
				failures++
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("pull: %d item(s) failed", failures)
	}
	return nil
}

// applyBlock fetches, decrypts, and merges one remote session block into the
// local store. Blocks already possessed (pushed or previously merged) are
// skipped. Chat is append-only, so the merge is the union of message histories;
// an unchanged local session is left untouched.
func (s *Syncer) applyBlock(ctx context.Context, e ManifestEntry) error {
	s.mu.Lock()
	_, possessed := s.have[e.BlockHash]
	s.mu.Unlock()
	if possessed {
		return nil
	}

	blob, err := s.client.GetBlock(ctx, e.BlockHash)
	if err != nil {
		return err
	}
	if got := session.HashCiphertext(blob); got != e.BlockHash {
		return fmt.Errorf("block hash mismatch: manifest %s, got %s", e.BlockHash, got)
	}
	plaintext, err := s.codec.Decode(blob)
	if err != nil {
		return fmt.Errorf("decrypting block: %w", err)
	}
	remote, err := session.UnmarshalSession(plaintext)
	if err != nil {
		return err
	}

	local, lerr := s.store.Load(e.ItemID)
	haveLocal := lerr == nil

	merged := remote
	if haveLocal {
		msgs := session.MergeMessages(local.Messages, remote.Messages)
		s.mu.Lock()
		s.have[e.BlockHash] = struct{}{}
		s.mu.Unlock()
		if len(msgs) == len(local.Messages) {
			// Local already contains everything this block has; nothing to
			// write, but record convergence so we do not re-push it.
			s.markSynced(local)
			return nil
		}
		merged = local
		merged.Messages = msgs
	} else {
		s.mu.Lock()
		s.have[e.BlockHash] = struct{}{}
		s.mu.Unlock()
	}
	if merged.Created.IsZero() {
		merged.Created = time.Now()
	}
	merged.Updated = time.Now()
	if err := s.store.Save(merged); err != nil {
		return fmt.Errorf("saving merged session: %w", err)
	}
	s.markSynced(merged)
	return nil
}

// applyTombstone deletes a session locally if the store supports deletion.
func (s *Syncer) applyTombstone(id string) error {
	d, ok := s.store.(deleter)
	if !ok {
		s.log.Warn("store does not support delete; skipping tombstone", "id", id)
		return nil
	}
	if err := d.Delete(id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.pushed, id)
	s.mu.Unlock()
	return nil
}

// verifyNuke returns true only when the manifest's nuke marker carries a valid
// Ed25519 signature over the canonical NUKE payload AND the signing public key
// is exactly this namespace's write key (derived from our K_folder). Either
// check failing means a malicious or buggy hub, so the marker is ignored rather
// than acted on — this is the T11 guard: the hub can serve arbitrary bytes but
// cannot forge the holder-of-key proof a real fleet-wide nuke requires.
func (s *Syncer) verifyNuke(m Manifest) bool {
	// The advertised pubkey must be OUR namespace write key; anything else is a
	// nuke ordered by (or forged for) a different key and is not ours to honor.
	if m.NukePubkey != session.DeriveWritePublicKey(s.cfg.Key) {
		return false
	}
	pub, err := base64.RawURLEncoding.DecodeString(m.NukePubkey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(m.NukeSig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), []byte(nukePayload(s.cfg.SyncID)), sig)
}

// handleNuke reacts to a verified fleet-wide nuke: it runs the caller-provided
// self-destruct callback (crypto-shred + purge) when one is set, then signals
// ErrNamespaceNuked so both a one-shot Sync caller and the background Run loop
// learn the namespace is gone and stop. A callback error is returned as-is (not
// as ErrNamespaceNuked) so the self-destruct is retried on the next tick.
func (s *Syncer) handleNuke(ctx context.Context) error {
	if s.onNuke != nil {
		if err := s.onNuke(ctx); err != nil {
			return fmt.Errorf("nuke self-destruct: %w", err)
		}
	}
	return ErrNamespaceNuked
}

// markSynced records a session's current plaintext hash so the next push skips
// it (both sides have converged), preventing a push/pull ping-pong.
func (s *Syncer) markSynced(sess korai.Session) {
	plaintext, err := session.MarshalSession(sess)
	if err != nil {
		return
	}
	h := contentHash(plaintext)
	s.mu.Lock()
	s.pushed[sess.ID] = h
	s.mu.Unlock()
}

// loadCursor reads the persisted manifest cursor; a missing cursor is 0.
func (s *Syncer) loadCursor() (int64, error) {
	data, err := os.ReadFile(s.cfg.CursorPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading sync cursor: %w", err)
	}
	var v int64
	if _, err := fmt.Sscanf(string(data), "%d", &v); err != nil {
		s.log.Warn("unreadable sync cursor; restarting from 0", "error", err)
		return 0, nil
	}
	return v, nil
}

// saveCursor persists the manifest cursor for the next run.
func (s *Syncer) saveCursor(v int64) error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.CursorPath), 0o700); err != nil {
		return fmt.Errorf("creating sync dir: %w", err)
	}
	if err := os.WriteFile(s.cfg.CursorPath, []byte(fmt.Sprintf("%d", v)), cursorFilePerm); err != nil {
		return fmt.Errorf("writing sync cursor: %w", err)
	}
	return nil
}

// contentHash is the hex SHA-256 of plaintext bytes, used as a local
// change-detection key (distinct from the ciphertext block hash sent to the hub).
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sizeBuckets is the padding ladder shared with the browser sync client. Block
// plaintext is padded up to the next bucket so the hub — which sees each block's
// ciphertext length — cannot infer conversation length from the raw size
// (attack model T3). The ladder MUST match the browser's for uniform metadata,
// but padding is per-surface and reader-tolerant (json.Unmarshal ignores the
// trailing spaces), so exact cross-surface byte-agreement is not required.
var sizeBuckets = [...]int{2048, 8192, 32768, 131072, 524288, 2097152}

// padToBucket returns b padded with trailing ASCII spaces (0x20) so its length
// rounds UP to the next value in sizeBuckets. Bytes already at or beyond the
// top bucket (2 MiB) are returned unchanged. Trailing spaces are legal JSON
// whitespace, so a padded canonical-JSON block still unmarshals to the same
// value with no decode-side change.
func padToBucket(b []byte) []byte {
	for _, bucket := range sizeBuckets {
		if len(b) <= bucket {
			if len(b) == bucket {
				return b
			}
			padded := make([]byte, bucket)
			n := copy(padded, b)
			for i := n; i < bucket; i++ {
				padded[i] = ' '
			}
			return padded
		}
	}
	return b
}

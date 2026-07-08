package synchub

import (
	"context"
	"crypto/sha256"
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
		client: NewClient(cfg.URL, cfg.SyncID, nil),
		store:  store,
		codec:  codec,
		log:    logger,
		pushed: make(map[string]string),
		have:   make(map[string]struct{}),
	}, nil
}

// Run drives Sync on a ticker until ctx is cancelled. Start it in a goroutine. A
// nil Syncer returns immediately. Tick failures are logged and retried on the
// next tick; they never stop the loop.
func (s *Syncer) Run(ctx context.Context) {
	if s == nil {
		return
	}
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("initial sync failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
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

		blob, err := s.codec.Encode(plaintext)
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

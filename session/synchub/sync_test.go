package synchub_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	korai "github.com/korai-one/korai-sdk-go"
	"github.com/korai-one/korai-sdk-go/session"
	"github.com/korai-one/korai-sdk-go/session/synchub"
)

// mockHub is a minimal in-memory implementation of the blind sync hub contract
// (docs/HISTORY_SYNC.md §11): opaque bearer namespace, content-addressed blocks,
// a monotonic manifest log, tombstones, and namespace wipe.
type mockHub struct {
	mu        sync.Mutex
	seq       int64
	blocks    map[string][]byte // hash -> ciphertext
	log       []logEntry        // append-only manifest log
	lastBear  string
	putCalls  int
	wipeCalls int
}

type logEntry struct {
	ItemID    string
	BlockHash string
	Seq       int64
	ByteSize  int64
	Tombstone bool
}

func newMockHub() *mockHub { return &mockHub{blocks: map[string][]byte{}} }

func (h *mockHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastBear = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/v1/sync/blocks":
		var req struct {
			ItemID     string `json:"item_id"`
			BlockHash  string `json:"block_hash"`
			Ciphertext string `json:"ciphertext"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		ct, _ := base64.StdEncoding.DecodeString(req.Ciphertext)
		h.putCalls++
		h.blocks[req.BlockHash] = ct
		h.seq++
		h.log = append(h.log, logEntry{ItemID: req.ItemID, BlockHash: req.BlockHash, Seq: h.seq, ByteSize: int64(len(ct))})
		writeJSON(w, map[string]int64{"seq": h.seq})

	case r.Method == http.MethodPost && r.URL.Path == "/v1/sync/tombstone":
		var req struct {
			ItemID string `json:"item_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.seq++
		h.log = append(h.log, logEntry{ItemID: req.ItemID, Seq: h.seq, Tombstone: true})
		writeJSON(w, map[string]int64{"seq": h.seq})

	case r.Method == http.MethodGet && r.URL.Path == "/v1/sync/manifest":
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		var entries []map[string]any
		var next int64 = since
		for _, e := range h.log {
			if e.Seq <= since {
				continue
			}
			entries = append(entries, map[string]any{
				"item_id":    e.ItemID,
				"block_hash": e.BlockHash,
				"seq":        e.Seq,
				"byte_size":  e.ByteSize,
				"tombstone":  e.Tombstone,
			})
			if e.Seq > next {
				next = e.Seq
			}
		}
		writeJSON(w, map[string]any{"entries": entries, "next": next})

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sync/blocks/"):
		hash := strings.TrimPrefix(r.URL.Path, "/v1/sync/blocks/")
		ct, ok := h.blocks[hash]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(ct)

	case r.Method == http.MethodDelete && r.URL.Path == "/v1/sync":
		h.wipeCalls++
		h.blocks = map[string][]byte{}
		h.log = nil
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func cfgFor(t *testing.T, url string) synchub.Config {
	t.Helper()
	key := testKey()
	return synchub.Config{
		Enabled:    true,
		URL:        url,
		SyncID:     session.DeriveSyncID(key),
		Key:        key,
		Interval:   time.Second,
		CursorPath: filepath.Join(t.TempDir(), "cursor"),
	}
}

func makeSession(id, text string) korai.Session {
	return korai.Session{
		ID:      id,
		Created: time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC),
		Updated: time.Now(),
		CWD:     "/w",
		Model:   "auto",
		Tool:    "test",
		Messages: []korai.SessionMessage{
			{Role: "user", Blocks: []korai.Block{korai.TextBlock{Text: text}}},
		},
	}
}

// TestPushThenPull pushes a session from store A to the hub, then confirms an
// empty store B pulls and reconstructs it (the ciphertext is unreadable to the
// hub but decodable by B, which holds the same key).
func TestPushThenPull(t *testing.T) {
	hub := newMockHub()
	srv := httptest.NewServer(hub)
	defer srv.Close()
	ctx := context.Background()

	storeA := session.NewFileStore(t.TempDir())
	if err := storeA.Save(makeSession("s1", "hello from A")); err != nil {
		t.Fatalf("save A: %v", err)
	}
	syncA, err := synchub.New(cfgFor(t, srv.URL), storeA, nil)
	if err != nil {
		t.Fatalf("new A: %v", err)
	}
	if err := syncA.Sync(ctx); err != nil {
		t.Fatalf("sync A: %v", err)
	}
	if hub.putCalls != 1 {
		t.Fatalf("expected 1 block pushed, got %d", hub.putCalls)
	}
	if hub.lastBear != session.DeriveSyncID(testKey()) {
		t.Fatalf("bearer mismatch: %q", hub.lastBear)
	}

	storeB := session.NewFileStore(t.TempDir())
	syncB, err := synchub.New(cfgFor(t, srv.URL), storeB, nil)
	if err != nil {
		t.Fatalf("new B: %v", err)
	}
	if err := syncB.Sync(ctx); err != nil {
		t.Fatalf("sync B: %v", err)
	}
	got, err := storeB.Load("s1")
	if err != nil {
		t.Fatalf("B load: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(got.Messages))
	}
	tb := got.Messages[0].Blocks[0].(korai.TextBlock)
	if tb.Text != "hello from A" {
		t.Fatalf("pulled text = %q", tb.Text)
	}
}

// TestHubStoresCiphertextOnly verifies the hub never sees plaintext: the stored
// block does not contain the message text.
func TestHubStoresCiphertextOnly(t *testing.T) {
	hub := newMockHub()
	srv := httptest.NewServer(hub)
	defer srv.Close()

	storeA := session.NewFileStore(t.TempDir())
	_ = storeA.Save(makeSession("s1", "TOPSECRETPAYLOAD"))
	syncA, _ := synchub.New(cfgFor(t, srv.URL), storeA, nil)
	if err := syncA.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, ct := range hub.blocks {
		if strings.Contains(string(ct), "TOPSECRETPAYLOAD") {
			t.Fatal("hub block contains plaintext")
		}
	}
}

// TestTombstonePropagates confirms a delete on A tombstones on the hub and the
// pull applies it to B.
func TestTombstonePropagates(t *testing.T) {
	hub := newMockHub()
	srv := httptest.NewServer(hub)
	defer srv.Close()
	ctx := context.Background()

	dirA := t.TempDir()
	storeA := session.NewFileStore(dirA)
	_ = storeA.Save(makeSession("s1", "doomed"))
	syncA, _ := synchub.New(cfgFor(t, srv.URL), storeA, nil)
	if err := syncA.Sync(ctx); err != nil {
		t.Fatalf("sync A push: %v", err)
	}

	// B pulls the session first.
	storeB := session.NewFileStore(t.TempDir())
	syncB, _ := synchub.New(cfgFor(t, srv.URL), storeB, nil)
	if err := syncB.Sync(ctx); err != nil {
		t.Fatalf("sync B pull: %v", err)
	}
	if _, err := storeB.Load("s1"); err != nil {
		t.Fatalf("B should have s1: %v", err)
	}

	// A deletes locally, then syncs → hub tombstone.
	if err := storeA.Delete("s1"); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	if err := syncA.Sync(ctx); err != nil {
		t.Fatalf("sync A tombstone: %v", err)
	}

	// B pulls again and applies the tombstone.
	if err := syncB.Sync(ctx); err != nil {
		t.Fatalf("sync B tombstone: %v", err)
	}
	if _, err := storeB.Load("s1"); err == nil {
		t.Fatal("expected s1 deleted on B after tombstone")
	}
}

// TestWipeRemote checks the namespace-wide delete backing the duress wipe.
func TestWipeRemote(t *testing.T) {
	hub := newMockHub()
	srv := httptest.NewServer(hub)
	defer srv.Close()

	c := synchub.NewClient(srv.URL, "bearer", nil)
	if _, err := c.PutBlock(context.Background(), "s1", "h1", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := c.WipeRemote(context.Background()); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if hub.wipeCalls != 1 || len(hub.blocks) != 0 {
		t.Fatalf("wipe not applied: calls=%d blocks=%d", hub.wipeCalls, len(hub.blocks))
	}
}

// TestDisabledSyncerIsNoop confirms a disabled config yields a nil Syncer whose
// methods are safe no-ops.
func TestDisabledSyncerIsNoop(t *testing.T) {
	s, err := synchub.New(synchub.Config{Enabled: false}, session.NewFileStore(t.TempDir()), nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if s != nil {
		t.Fatal("expected nil Syncer when disabled")
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("nil Sync should be no-op: %v", err)
	}
}

// TestManifestPaging exercises the client's manifest read directly.
func TestManifestPaging(t *testing.T) {
	hub := newMockHub()
	srv := httptest.NewServer(hub)
	defer srv.Close()
	c := synchub.NewClient(srv.URL, "b", nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.PutBlock(ctx, "item"+strconv.Itoa(i), "h"+strconv.Itoa(i), []byte{byte(i)}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	m, err := c.Manifest(ctx, 0, 100)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(m.Entries) != 3 || m.Next != 3 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	// sanity: entries are the ones we put
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Seq < m.Entries[j].Seq })
	if m.Entries[0].ItemID != "item0" {
		t.Fatalf("first entry = %q", m.Entries[0].ItemID)
	}
}

package korai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

// AuditEntry is one row in the append-only audit log. The chain
// fields (PrevHash / RowHash) reproduce the orchestrator's audit
// schema so callers can later sync entries up to Korai Cloud
// without schema drift.
type AuditEntry struct {
	ID             string                 `json:"id"`
	SequenceNo     int                    `json:"sequence_no"`
	OrganizationID string                 `json:"organization_id,omitempty"`
	EventType      string                 `json:"event_type"`
	UserID         string                 `json:"user_id,omitempty"`
	ResourceType   string                 `json:"resource_type,omitempty"`
	ResourceID     string                 `json:"resource_id,omitempty"`
	Payload        map[string]any         `json:"payload,omitempty"`
	IPAddress      string                 `json:"ip_address,omitempty"`
	Severity       string                 `json:"severity"`
	PrevHash       string                 `json:"prev_hash,omitempty"`
	RowHash        string                 `json:"row_hash,omitempty"`
	Signature      string                 `json:"signature,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
}

// AuditFilter narrows down a List() query. Zero values are ignored.
type AuditFilter struct {
	OrganizationID string
	EventType      string
	ResourceID     string
	UserID         string
	Severity       string
	From           time.Time
	To             time.Time
	Limit          int
}

// AuditStore is the storage backend used by AuditModule. The default
// implementation is InMemoryAuditStore; production callers can plug
// in a Postgres / SQLite-backed implementation by satisfying this
// interface and passing it via WithAuditStore.
type AuditStore interface {
	// Append persists a fully-formed entry. Implementations must
	// preserve ordering: subsequent calls to List for the same
	// organization should return entries sorted by SequenceNo.
	Append(ctx context.Context, entry AuditEntry) error
	// List returns entries matching filter. Newest-first when
	// Limit is set, oldest-first otherwise.
	List(ctx context.Context, filter AuditFilter) ([]AuditEntry, error)
	// Last returns the most recent entry for an organization, used
	// by the module to compute prev_hash. Returns (nil, nil) when
	// the chain is empty.
	Last(ctx context.Context, organizationID string) (*AuditEntry, error)
	// Count returns the total number of entries for an organization.
	Count(ctx context.Context, organizationID string) (int, error)
}

// AuditModule is the SDK-side audit log. It hashes entries into a
// chain (PrevHash → RowHash) so tampering can be detected via
// VerifyChain.
type AuditModule struct {
	store AuditStore
}

// NewAuditModule wraps a store with chain bookkeeping.
func NewAuditModule(store AuditStore) *AuditModule {
	return &AuditModule{store: store}
}

// Store returns the underlying store. Useful for tests.
func (a *AuditModule) Store() AuditStore { return a.store }

// Log appends a new entry, computing PrevHash / RowHash. The entry
// is enriched with a generated ID and CreatedAt timestamp before
// being persisted. The committed entry is returned.
func (a *AuditModule) Log(ctx context.Context, entry AuditEntry) (*AuditEntry, error) {
	if entry.EventType == "" {
		return nil, errors.New("korai: audit entry needs an event_type")
	}
	if entry.Severity == "" {
		entry.Severity = "info"
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	if entry.ID == "" {
		entry.ID = newAuditID(entry.CreatedAt, entry.EventType)
	}

	last, err := a.store.Last(ctx, entry.OrganizationID)
	if err != nil {
		return nil, err
	}
	if last != nil {
		entry.PrevHash = last.RowHash
		entry.SequenceNo = last.SequenceNo + 1
	} else {
		entry.SequenceNo = 1
	}
	entry.RowHash = computeRowHash(entry)

	if err := a.store.Append(ctx, entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

// List returns entries matching the filter.
func (a *AuditModule) List(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	return a.store.List(ctx, filter)
}

// VerifyChain re-computes every row hash in order and reports
// (isValid, checked_count, error). When the chain is broken the
// returned count is the index (1-based) of the first invalid entry.
func (a *AuditModule) VerifyChain(ctx context.Context, organizationID string) (bool, int, error) {
	entries, err := a.store.List(ctx, AuditFilter{OrganizationID: organizationID})
	if err != nil {
		return false, 0, err
	}
	// Ensure ascending order regardless of store-side sorting.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SequenceNo < entries[j].SequenceNo
	})
	prev := ""
	for i, e := range entries {
		if e.PrevHash != prev {
			return false, i + 1, nil
		}
		expected := computeRowHash(e)
		if expected != e.RowHash {
			return false, i + 1, nil
		}
		prev = e.RowHash
	}
	return true, len(entries), nil
}

// computeRowHash mirrors the SHA-256 over canonical-JSON algorithm
// used in vertical-fiduciaire/backend/services/audit.py. The
// signature field is excluded so re-signing doesn't break the
// chain.
func computeRowHash(e AuditEntry) string {
	clone := e
	clone.RowHash = ""
	clone.Signature = ""
	raw, _ := json.Marshal(struct {
		SequenceNo     int            `json:"sequence_no"`
		OrganizationID string         `json:"organization_id"`
		EventType      string         `json:"event_type"`
		UserID         string         `json:"user_id"`
		ResourceType   string         `json:"resource_type"`
		ResourceID     string         `json:"resource_id"`
		Payload        map[string]any `json:"payload"`
		Severity       string         `json:"severity"`
		PrevHash       string         `json:"prev_hash"`
		CreatedAt      string         `json:"created_at"`
	}{
		SequenceNo:     clone.SequenceNo,
		OrganizationID: clone.OrganizationID,
		EventType:      clone.EventType,
		UserID:         clone.UserID,
		ResourceType:   clone.ResourceType,
		ResourceID:     clone.ResourceID,
		Payload:        clone.Payload,
		Severity:       clone.Severity,
		PrevHash:       clone.PrevHash,
		CreatedAt:      clone.CreatedAt.UTC().Format(time.RFC3339Nano),
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// newAuditID generates a stable, sortable ID for an entry. Format
// "audit-<unixnano>-<short>" — collision-resistant within a
// process and chronologically ordered.
func newAuditID(ts time.Time, eventType string) string {
	sum := sha256.Sum256([]byte(eventType))
	return "audit-" + ts.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(sum[:4])
}

// --- InMemoryAuditStore ------------------------------------------------------

// InMemoryAuditStore is the default AuditStore — entries are kept
// in a slice keyed by organization. Safe for concurrent use, and
// suitable for tests / dev. Replace with a persistent store in
// production via WithAuditStore.
type InMemoryAuditStore struct {
	mu      sync.RWMutex
	byOrg   map[string][]AuditEntry
}

// NewInMemoryAuditStore returns an empty store ready to use.
func NewInMemoryAuditStore() *InMemoryAuditStore {
	return &InMemoryAuditStore{byOrg: make(map[string][]AuditEntry)}
}

// Append implements AuditStore.
func (s *InMemoryAuditStore) Append(ctx context.Context, entry AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byOrg[entry.OrganizationID] = append(s.byOrg[entry.OrganizationID], entry)
	return nil
}

// List implements AuditStore.
func (s *InMemoryAuditStore) List(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	src := s.byOrg[filter.OrganizationID]
	out := make([]AuditEntry, 0, len(src))
	for _, e := range src {
		if !matchesFilter(e, filter) {
			continue
		}
		out = append(out, e)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		// Newest-first when limited.
		sort.Slice(out, func(i, j int) bool {
			return out[i].SequenceNo > out[j].SequenceNo
		})
		out = out[:filter.Limit]
	}
	return out, nil
}

// Last implements AuditStore.
func (s *InMemoryAuditStore) Last(ctx context.Context, organizationID string) (*AuditEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.byOrg[organizationID]
	if len(src) == 0 {
		return nil, nil
	}
	last := src[len(src)-1]
	return &last, nil
}

// Count implements AuditStore.
func (s *InMemoryAuditStore) Count(ctx context.Context, organizationID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byOrg[organizationID]), nil
}

func matchesFilter(e AuditEntry, f AuditFilter) bool {
	if f.EventType != "" && e.EventType != f.EventType {
		return false
	}
	if f.ResourceID != "" && e.ResourceID != f.ResourceID {
		return false
	}
	if f.UserID != "" && e.UserID != f.UserID {
		return false
	}
	if f.Severity != "" && e.Severity != f.Severity {
		return false
	}
	if !f.From.IsZero() && e.CreatedAt.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.CreatedAt.After(f.To) {
		return false
	}
	return true
}

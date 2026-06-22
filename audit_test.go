package korai

import (
	"context"
	"testing"
	"time"
)

func TestAuditLogAppendsAndChains(t *testing.T) {
	mod := NewAuditModule(NewInMemoryAuditStore())
	ctx := context.Background()

	first, err := mod.Log(ctx, AuditEntry{
		EventType:      "user_login",
		OrganizationID: "org-1",
		UserID:         "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.SequenceNo != 1 {
		t.Fatalf("seq = %d", first.SequenceNo)
	}
	if first.PrevHash != "" {
		t.Fatalf("first prev_hash should be empty, got %q", first.PrevHash)
	}
	if first.RowHash == "" {
		t.Fatal("row hash missing")
	}

	second, err := mod.Log(ctx, AuditEntry{
		EventType:      "doc_created",
		OrganizationID: "org-1",
		UserID:         "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.SequenceNo != 2 {
		t.Fatalf("seq = %d", second.SequenceNo)
	}
	if second.PrevHash != first.RowHash {
		t.Fatalf("chain broken: prev=%q row=%q", second.PrevHash, first.RowHash)
	}
}

func TestAuditLogRequiresEventType(t *testing.T) {
	mod := NewAuditModule(NewInMemoryAuditStore())
	_, err := mod.Log(context.Background(), AuditEntry{OrganizationID: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAuditVerifyChainHonoursTampering(t *testing.T) {
	store := NewInMemoryAuditStore()
	mod := NewAuditModule(store)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := mod.Log(ctx, AuditEntry{
			EventType:      "x",
			OrganizationID: "org-1",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ok, count, err := mod.VerifyChain(ctx, "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || count != 3 {
		t.Fatalf("verify = (%v, %d)", ok, count)
	}

	// Tamper with the second entry directly in the store.
	store.byOrg["org-1"][1].EventType = "tampered"

	ok, _, err = mod.VerifyChain(ctx, "org-1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected tamper detection")
	}
}

func TestAuditListFilters(t *testing.T) {
	mod := NewAuditModule(NewInMemoryAuditStore())
	ctx := context.Background()
	for i, ev := range []string{"a", "b", "a"} {
		_, err := mod.Log(ctx, AuditEntry{
			EventType:      ev,
			OrganizationID: "o",
			UserID:         "u",
			CreatedAt:      time.Now().UTC().Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	all, err := mod.List(ctx, AuditFilter{OrganizationID: "o"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d", len(all))
	}
	onlyA, err := mod.List(ctx, AuditFilter{OrganizationID: "o", EventType: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyA) != 2 {
		t.Fatalf("expected 2 a's, got %d", len(onlyA))
	}
}

func TestAuditLogDefaultsSeverity(t *testing.T) {
	mod := NewAuditModule(NewInMemoryAuditStore())
	e, err := mod.Log(context.Background(), AuditEntry{
		EventType:      "x",
		OrganizationID: "o",
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Severity != "info" {
		t.Fatalf("severity = %q", e.Severity)
	}
}

func TestInMemoryAuditStoreCount(t *testing.T) {
	store := NewInMemoryAuditStore()
	mod := NewAuditModule(store)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = mod.Log(ctx, AuditEntry{EventType: "x", OrganizationID: "o"})
	}
	n, err := store.Count(ctx, "o")
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("count = %d", n)
	}
}

func TestAuditModuleStoreAccessor(t *testing.T) {
	store := NewInMemoryAuditStore()
	mod := NewAuditModule(store)
	if mod.Store() != store {
		t.Fatal("Store accessor mismatch")
	}
}

package korai

import (
	"context"
	"errors"
	"testing"
)

func TestSwitchOrganizationUpdatesHeader(t *testing.T) {
	cli := New()
	if err := cli.SwitchOrganization(context.Background(), "org-9"); err != nil {
		t.Fatal(err)
	}
	if cli.OrganizationID() != "org-9" {
		t.Fatalf("org = %q", cli.OrganizationID())
	}
}

func TestSwitchOrganizationRejectsEmpty(t *testing.T) {
	cli := New()
	err := cli.SwitchOrganization(context.Background(), "")
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestListMyOrganizationsStubbed(t *testing.T) {
	cli := New()
	if _, err := cli.ListMyOrganizations(context.Background()); !IsNotImplemented(err) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}

func TestGetCurrentOrganizationStubbed(t *testing.T) {
	cli := New()
	if _, err := cli.GetCurrentOrganization(context.Background()); !IsNotImplemented(err) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}

func TestListMembersStubbed(t *testing.T) {
	cli := New()
	if _, err := cli.ListMembers(context.Background()); !IsNotImplemented(err) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}

func TestInviteMemberStubbed(t *testing.T) {
	cli := New()
	if err := cli.InviteMember(context.Background(), "x@k.io", "admin"); !IsNotImplemented(err) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}

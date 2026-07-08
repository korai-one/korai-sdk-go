package korai

import (
	"context"
)

// Organization is a tenant scope (cabinet, hospital, studio, …).
type Organization struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Country     string `json:"country,omitempty"`
	Industry    string `json:"industry,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	MemberCount int    `json:"member_count,omitempty"`
}

// Membership describes the current user's role inside an
// Organization. Roles mirror the Python SDK enum:
//
//	admin, partner, collaborator, accountant, reader.
type Membership struct {
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	Role             string `json:"role"`
	IsActive         bool   `json:"is_active"`
}

// ListMyOrganizations returns the organizations the authenticated
// user belongs to.
//
// TODO(cloud): the orchestrator does not yet expose a tenant API —
// it carries a single user-organization pairing through the JWT
// claims. Returns ErrNotImplemented until /tenant/me ships.
func (c *Client) ListMyOrganizations(ctx context.Context) ([]Organization, error) {
	return nil, ErrNotImplemented
}

// GetCurrentOrganization returns the organization the current
// session is scoped to.
//
// TODO(cloud): pending tenant API.
func (c *Client) GetCurrentOrganization(ctx context.Context) (*Organization, error) {
	return nil, ErrNotImplemented
}

// SwitchOrganization changes the X-Korai-Organization header used by
// future requests. The change is purely client-side until the
// orchestrator's tenant API ships — at which point this will also
// re-issue the token.
func (c *Client) SwitchOrganization(ctx context.Context, organizationID string) error {
	if organizationID == "" {
		return ErrInvalidConfig
	}
	c.SetOrganizationID(organizationID)
	return nil
}

// ListMembers returns the members of the current organization.
//
// TODO(cloud): pending tenant API.
func (c *Client) ListMembers(ctx context.Context) ([]Membership, error) {
	return nil, ErrNotImplemented
}

// InviteMember invites someone to the current organization.
//
// TODO(cloud): pending tenant API.
func (c *Client) InviteMember(ctx context.Context, email, role string) error {
	return ErrNotImplemented
}

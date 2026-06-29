package korai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/korai-one/korai-sdk-go/koraiapi"
)

// CreditPackage describes a purchasable bundle of credits, mirroring
// the orchestrator's CreditPackage type emitted by GET
// /billing/packages.
type CreditPackage struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	CreditsEUR float64 `json:"credits_eur"`
	PriceCents int     `json:"price_cents"`
}

// Transaction is one row in the user's billing ledger.
type Transaction struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	AmountEUR   float64   `json:"amount_eur"`
	Description string    `json:"description"`
	Reference   string    `json:"reference,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// CheckoutSession is the URL returned by POST /billing/checkout
// that the dashboard redirects to.
type CheckoutSession struct {
	URL string `json:"checkout_url"`
}

// GetBalance returns the authenticated user's credit balance in
// EUR.
func (c *Client) GetBalance(ctx context.Context) (float64, error) {
	httpResp, err := c.gen.GetBalance(ctx)
	var decoded struct {
		BalanceEUR float64 `json:"balance_eur"`
	}
	if err := c.genDecode(httpResp, err, &decoded); err != nil {
		return 0, err
	}
	return decoded.BalanceEUR, nil
}

// ListPackages returns the available credit bundles.
func (c *Client) ListPackages(ctx context.Context) ([]CreditPackage, error) {
	httpResp, err := c.gen.ListPackages(ctx)
	var decoded struct {
		Packages []CreditPackage `json:"packages"`
	}
	if err := c.genDecode(httpResp, err, &decoded); err != nil {
		return nil, err
	}
	return decoded.Packages, nil
}

// ListTransactions returns the user's recent billing transactions.
// `limit` is forwarded to the orchestrator's `limit` query
// parameter; pass 0 for the default (50).
func (c *Client) ListTransactions(ctx context.Context, limit int) ([]Transaction, error) {
	var params koraiapi.ListTransactionsParams
	if limit > 0 {
		params.Limit = &limit
	}
	httpResp, err := c.gen.ListTransactions(ctx, &params)
	var decoded struct {
		Transactions []Transaction `json:"transactions"`
	}
	if err := c.genDecode(httpResp, err, &decoded); err != nil {
		return nil, err
	}
	return decoded.Transactions, nil
}

// CreateCheckout creates a Stripe Checkout Session for the given
// package and returns the URL to redirect the user to.
func (c *Client) CreateCheckout(ctx context.Context, packageID string) (*CheckoutSession, error) {
	if packageID == "" {
		return nil, fmt.Errorf("%w: package_id is required", ErrInvalidConfig)
	}
	raw, err := json.Marshal(map[string]string{"package_id": packageID})
	if err != nil {
		return nil, fmt.Errorf("korai: marshal request body: %w", err)
	}
	httpResp, err := c.gen.CreateCheckoutWithBody(ctx, "application/json", bytes.NewReader(raw))
	var out CheckoutSession
	if err := c.genDecode(httpResp, err, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSubscription returns the current subscription (anonymous tier
// or org subscription) for the authenticated user.
//
// TODO(cloud): only the anonymous-tier subscription endpoint is
// public today; an org-aware subscription resource is on the
// roadmap. Returns ErrNotImplemented for now.
func (c *Client) GetSubscription(ctx context.Context) (any, error) {
	return nil, ErrNotImplemented
}

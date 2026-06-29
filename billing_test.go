package korai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/balance" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"balance_eur": 12.34}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	bal, err := cli.GetBalance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bal != 12.34 {
		t.Fatalf("balance = %v", bal)
	}
}

func TestListPackages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"packages":[
			{"id":"starter","label":"Starter — 5 €","credits_eur":5,"price_cents":500},
			{"id":"plus","label":"Plus — 20 €","credits_eur":20,"price_cents":2000}
		]}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	pkgs, err := cli.ListPackages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 || pkgs[0].ID != "starter" {
		t.Fatalf("unexpected packages: %#v", pkgs)
	}
}

func TestListTransactionsForwardsLimit(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RawQuery
		w.Write([]byte(`{"transactions": []}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	if _, err := cli.ListTransactions(context.Background(), 25); err != nil {
		t.Fatal(err)
	}
	if seen != "limit=25" {
		t.Fatalf("query = %q", seen)
	}
}

func TestCreateCheckoutHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/checkout" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"checkout_url": "https://stripe.test/abc"}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	sess, err := cli.CreateCheckout(context.Background(), "starter")
	if err != nil {
		t.Fatal(err)
	}
	if sess.URL != "https://stripe.test/abc" {
		t.Fatalf("url = %q", sess.URL)
	}
}

func TestCreateCheckoutRequiresPackageID(t *testing.T) {
	cli := New()
	_, err := cli.CreateCheckout(context.Background(), "")
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestGetSubscriptionIsNotImplemented(t *testing.T) {
	cli := New()
	_, err := cli.GetSubscription(context.Background())
	if !IsNotImplemented(err) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}

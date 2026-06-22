package korai

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginStoresToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if body["email"] != "u@k.io" || body["password"] != "secret123" {
			http.Error(w, "{}", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"user":{"id":"u1","email":"u@k.io","display_name":"U"},"token":"jwt-1"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	tp, err := cli.Login(context.Background(), "u@k.io", "secret123")
	if err != nil {
		t.Fatal(err)
	}
	if tp.AccessToken != "jwt-1" {
		t.Fatalf("token = %q", tp.AccessToken)
	}
	if cli.APIKey() != "jwt-1" {
		t.Fatalf("client did not store token, got %q", cli.APIKey())
	}
}

func TestLoginReturnsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad credentials","type":"authentication_error"}}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	_, err := cli.Login(context.Background(), "x", "y")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestRegisterPostsPayload(t *testing.T) {
	var seen RegisterPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		w.Write([]byte(`{"user":{"id":"u2","email":"a@b.io","display_name":"A"},"token":"jwt-new"}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL))
	tp, err := cli.Register(context.Background(), RegisterPayload{
		Email:       "a@b.io",
		Password:    "longpasswd",
		DisplayName: "A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tp.AccessToken != "jwt-new" {
		t.Fatalf("token = %q", tp.AccessToken)
	}
	if seen.Email != "a@b.io" {
		t.Fatalf("server saw email = %q", seen.Email)
	}
}

func TestRegisterRequiresEmailAndPassword(t *testing.T) {
	cli := New()
	_, err := cli.Register(context.Background(), RegisterPayload{Email: "missing"})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
}

func TestMeReturnsUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jwt-x" {
			http.Error(w, `{}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"user":{"id":"u9","email":"me@k.io","display_name":"Me","tier":"pro","role":"admin"}}`))
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithAPIKey("jwt-x"))
	user, err := cli.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "me@k.io" || user.Tier != "pro" {
		t.Fatalf("unexpected user: %#v", user)
	}
}

func TestRefreshIsNotImplemented(t *testing.T) {
	cli := New()
	_, err := cli.Refresh(context.Background(), "tok")
	if !IsNotImplemented(err) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}

func TestLogoutClearsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cli := New(WithBaseURL(srv.URL), WithAPIKey("jwt-1"))
	if err := cli.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cli.APIKey() != "" {
		t.Fatalf("expected empty key, got %q", cli.APIKey())
	}
}

// makeJWT crafts an HS256 JWT with the given claims and secret. It's
// only used for ParseJWT tests — production code never builds tokens
// client-side.
func makeJWT(t *testing.T, claims JWTClaims, secret string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := header + "." + body
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + sig
}

func TestParseJWTDecodesClaims(t *testing.T) {
	tok := makeJWT(t, JWTClaims{
		Sub:   "user-1",
		Email: "x@k.io",
		Tier:  "pro",
		Iat:   time.Now().Unix(),
		Exp:   time.Now().Add(time.Hour).Unix(),
	}, "secret")
	claims, err := ParseJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Sub != "user-1" || claims.Email != "x@k.io" {
		t.Fatalf("claims = %#v", claims)
	}
	if claims.IsExpired() {
		t.Fatal("expected non-expired token")
	}
}

func TestParseJWTRejectsMalformed(t *testing.T) {
	if _, err := ParseJWT("not.a.real.jwt.here"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ParseJWT("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestJWTIsExpired(t *testing.T) {
	c := &JWTClaims{Exp: time.Now().Add(-time.Hour).Unix()}
	if !c.IsExpired() {
		t.Fatal("expected expired")
	}
	c2 := &JWTClaims{Exp: time.Now().Add(time.Hour).Unix()}
	if c2.IsExpired() {
		t.Fatal("expected not expired")
	}
}

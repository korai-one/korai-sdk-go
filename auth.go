package korai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// User mirrors the orchestrator's auth.User type. Only the fields
// exposed on the wire are surfaced — internal hashes and timestamps
// stay opaque.
type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Tier        string    `json:"tier,omitempty"`
	Role        string    `json:"role,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
}

// TokenPair holds the authentication credentials returned by the
// orchestrator. The current orchestrator only emits a single Bearer
// token (Korai uses long-lived sessions); RefreshToken / ExpiresIn
// are kept in the type so callers wired against a future refresh API
// don't need to migrate.
type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

// CurrentUser is what GET /auth/me returns. Wraps User with optional
// org metadata lifted from the JWT.
type CurrentUser struct {
	User
	OrganizationID string `json:"organization_id,omitempty"`
}

// JWTClaims is a partial view of the HS256 payload the orchestrator
// embeds in every token. Decoded by ParseJWT below; safe to consult
// without verifying the signature for client-side use.
type JWTClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email,omitempty"`
	Tier  string `json:"tier,omitempty"`
	Role  string `json:"role,omitempty"`
	Iat   int64  `json:"iat,omitempty"`
	Exp   int64  `json:"exp,omitempty"`
}

// RegisterPayload bundles the fields accepted by POST /auth/signup.
// The orchestrator's current schema is minimal (email, password,
// display_name) — the optional org/country fields are stubbed for
// forward-compatibility with the planned tenant-aware signup.
type RegisterPayload struct {
	Email                string `json:"email"`
	Password             string `json:"password"`
	DisplayName          string `json:"display_name"`
	OrganizationName     string `json:"organization_name,omitempty"`
	OrganizationCountry  string `json:"organization_country,omitempty"`
	PreferredLanguage    string `json:"preferred_language,omitempty"`
}

// loginEnvelope mirrors the orchestrator's `{"user":..., "token":...}`
// response. Login / Register adapt it to TokenPair so callers don't
// have to know about the wire shape.
type loginEnvelope struct {
	User  *User  `json:"user"`
	Token string `json:"token"`
}

// Login authenticates with email + password against
// POST /auth/login. On success the returned token is stored in the
// client (via UseToken) so subsequent calls are authenticated.
//
// Use errors.Is(err, korai.ErrUnauthorized) to detect bad
// credentials.
func (c *Client) Login(ctx context.Context, email, password string) (*TokenPair, error) {
	raw, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return nil, fmt.Errorf("korai: marshal request body: %w", err)
	}
	httpResp, err := c.gen.LoginWithBody(ctx, "application/json", bytes.NewReader(raw))
	var env loginEnvelope
	if err := c.genDecode(httpResp, err, &env); err != nil {
		return nil, err
	}
	if env.Token == "" {
		return nil, errors.New("korai: login response missing token")
	}
	c.UseToken(env.Token)
	return &TokenPair{AccessToken: env.Token, TokenType: "Bearer"}, nil
}

// Register creates a new account via POST /auth/signup. Mirrors
// Login on success (token stored, returned wrapped in TokenPair).
//
// errors.Is(err, korai.ErrUnauthorized) won't match here because
// /auth/signup answers 409 Conflict on duplicate emails. Inspect
// (*APIError).StatusCode for that case.
func (c *Client) Register(ctx context.Context, payload RegisterPayload) (*TokenPair, error) {
	if payload.Email == "" || payload.Password == "" {
		return nil, fmt.Errorf("%w: email and password are required", ErrInvalidConfig)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("korai: marshal request body: %w", err)
	}
	httpResp, err := c.gen.SignupWithBody(ctx, "application/json", bytes.NewReader(raw))
	var env loginEnvelope
	if err := c.genDecode(httpResp, err, &env); err != nil {
		return nil, err
	}
	if env.Token == "" {
		return nil, errors.New("korai: signup response missing token")
	}
	c.UseToken(env.Token)
	return &TokenPair{AccessToken: env.Token, TokenType: "Bearer"}, nil
}

// Me returns the currently authenticated user. Hits GET /auth/me; if
// the orchestrator is unreachable but a JWT is configured locally,
// callers may fall back to ParseJWT to decode the embedded claims.
func (c *Client) Me(ctx context.Context) (*CurrentUser, error) {
	httpResp, err := c.gen.GetCurrentUser(ctx)
	var env struct {
		User *CurrentUser `json:"user"`
	}
	if err := c.genDecode(httpResp, err, &env); err != nil {
		return nil, err
	}
	if env.User == nil {
		return nil, errors.New("korai: /auth/me response missing user")
	}
	return env.User, nil
}

// Refresh exchanges a refresh token for a new TokenPair. The
// orchestrator currently uses long-lived Bearer tokens with no
// refresh flow; this method returns ErrNotImplemented until that
// endpoint ships.
//
// TODO(cloud): implement once the orchestrator exposes /auth/refresh.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	return nil, ErrNotImplemented
}

// Logout is best-effort. JWTs are stateless so the local token is
// dropped from the client and any future-friendly POST /auth/logout
// is invoked when available. Errors from the network call are
// swallowed (logout is purely a UX concern).
func (c *Client) Logout(ctx context.Context) error {
	c.UseToken("")
	// The orchestrator has no /auth/logout today; the call is here so
	// the API stays stable when one ships.
	// We deliberately ignore the error.
	_ = c.doRequest(ctx, "POST", "/auth/logout", nil, nil)
	return nil
}

// ParseJWT decodes the claims embedded in a Korai JWT WITHOUT
// verifying the signature. Useful for client-side inspection
// (display name, tier badge). For trust decisions always call
// /auth/me on the orchestrator.
func ParseJWT(token string) (*JWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("korai: malformed JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some emitters use std base64 encoding — try it before giving up.
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("korai: decode JWT payload: %w", err)
		}
	}
	var claims JWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("korai: parse JWT claims: %w", err)
	}
	return &claims, nil
}

// IsExpired reports whether the JWT has passed its `exp` timestamp.
// Returns true on malformed tokens too — better safe than logged in.
func (j *JWTClaims) IsExpired() bool {
	if j == nil || j.Exp == 0 {
		return true
	}
	return time.Now().Unix() > j.Exp
}

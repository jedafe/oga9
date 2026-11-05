package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestOIDCProviderFlow — ADR-028/§29: create platform client → authorize
// (PKCE) → exchange → userinfo (global claims only) → refresh rotation
// → revoke. Code replay and refresh replay must both refuse.
func TestOIDCProviderFlow(t *testing.T) {
	svc := testSvc(t)
	ctx := context.Background()
	pool := svc.pool

	uid := uuid.New()
	if _, err := pool.Exec(ctx,
		"INSERT INTO users (id, email, password_hash, display_name, status, is_verified) VALUES ($1,$2,'x','IDP User','active',true)",
		uid, "idp-"+uuid.NewString()[:8]+"@x.test"); err != nil {
		t.Fatal(err)
	}

	c, secret, err := svc.CreatePlatformClient(ctx, OAuthClientIn{
		Name: "japa.to", RedirectURIs: []string{"https://japa.to/auth/cb"}, Trusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "platform" || c.OrganizationID.Valid {
		t.Fatal("platform client must carry no org context")
	}

	verifier := "test-verifier-" + uuid.NewString()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// unregistered redirect_uri refused before code issuance
	if _, err := svc.Authorize(ctx, uid, c.ClientID, "https://evil.example/cb", challenge, "S256"); !errors.Is(err, ErrBadRedirectURI) {
		t.Fatalf("unregistered redirect: %v", err)
	}
	code, err := svc.Authorize(ctx, uid, c.ClientID, "https://japa.to/auth/cb", challenge, "S256")
	if err != nil {
		t.Fatal(err)
	}

	// wrong client_secret refused
	if _, err := svc.ExchangeCode(ctx, c.ClientID, "wrong", code, "https://japa.to/auth/cb", verifier, "1.2.3.4", "ua"); !errors.Is(err, ErrBadClientSecret) {
		t.Fatalf("bad secret: %v", err)
	}
	res, err := svc.ExchangeCode(ctx, c.ClientID, secret, code, "https://japa.to/auth/cb", verifier, "1.2.3.4", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Fatal("missing tokens")
	}
	// code replay refused (consumed_at CAS)
	if _, err := svc.ExchangeCode(ctx, c.ClientID, secret, code, "https://japa.to/auth/cb", verifier, "1.2.3.4", "ua"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("code replay: %v", err)
	}

	// userinfo via the session jti behind the access token
	var jti pgtype.UUID
	if err := pool.QueryRow(ctx,
		"SELECT jti FROM user_sessions WHERE user_id=$1 ORDER BY created_at DESC LIMIT 1", uid).Scan(&jti); err != nil {
		t.Fatal(err)
	}
	claims, err := svc.UserinfoClaims(ctx, uuid.UUID(jti.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != uid.String() || claims["email"] == "" || claims["email_verified"] != true {
		t.Fatalf("bad claims: %v", claims)
	}
	if _, ok := claims["oga9_org_id"]; ok {
		t.Fatal("platform client leaked org-scoped claim")
	}

	// refresh rotates; the superseded refresh token dies
	res2, err := svc.RefreshGrant(ctx, c.ClientID, secret, res.RefreshToken, "1.2.3.4", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if res2.RefreshToken == res.RefreshToken {
		t.Fatal("refresh token not rotated")
	}
	if _, err := svc.RefreshGrant(ctx, c.ClientID, secret, res.RefreshToken, "1.2.3.4", "ua"); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("refresh replay: %v", err)
	}

	// revoke kills userinfo for the live grant
	var jti2 pgtype.UUID
	_ = pool.QueryRow(ctx,
		"SELECT jti FROM user_sessions WHERE user_id=$1 ORDER BY created_at DESC LIMIT 1", uid).Scan(&jti2)
	if err := svc.RevokeSessionGrant(ctx, uuid.UUID(jti2.Bytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UserinfoClaims(ctx, uuid.UUID(jti2.Bytes)); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("userinfo after revoke: %v", err)
	}

	// disabled client: status flip revokes grants + blocks authorize
	if err := svc.SetOAuthClientStatus(ctx, uuid.UUID(c.ID.Bytes), "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authorize(ctx, uid, c.ClientID, "https://japa.to/auth/cb", challenge, "S256"); !errors.Is(err, ErrClientDisabled) {
		t.Fatalf("authorize on disabled client: %v", err)
	}
}

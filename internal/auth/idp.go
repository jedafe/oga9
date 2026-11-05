// Ref: oga9-engineering/sections/29-wordpress-connector.md + ADR-028 —
// OGA9 as OIDC/OAuth2 provider: authorization-code + PKCE (S256) for
// connector (org-scoped) and platform (ecosystem relying party) clients.
// Issued tokens are ordinary §02 sessions bound to oauth_grants.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jedafe/oga9/internal/db"
)

var (
	ErrClientNotFound  = errors.New("oauth client not found")
	ErrClientDisabled  = errors.New("oauth client disabled")
	ErrBadRedirectURI  = errors.New("redirect_uri does not match a registered URI")
	ErrBadClientSecret = errors.New("client authentication failed")
	ErrCodeInvalid     = errors.New("authorization code invalid, expired, or consumed")
	ErrBadPKCE         = errors.New("PKCE verification failed")
	ErrAccessDenied    = errors.New("access_denied")
	ErrGrantInvalid    = errors.New("grant invalid or revoked")
)

const refreshGrantTTL = 30 * 24 * time.Hour

// ---------- client management (platform.clients.manage) ----------

type OAuthClientIn struct {
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
	Trusted      bool     `json:"trusted"`
}

// CreatePlatformClient registers an ecosystem relying party (kind
// 'platform' — no org context). Returns the row + the plaintext secret,
// shown once and never recoverable (bcrypt at rest).
func (s *Service) CreatePlatformClient(ctx context.Context, in OAuthClientIn) (*db.OauthClients, string, error) {
	if in.Name == "" || len(in.RedirectURIs) == 0 {
		return nil, "", ErrBadRedirectURI
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, "", err
	}
	secret := hex.EncodeToString(secretBytes)
	hash, err := HashPassword(secret)
	if err != nil {
		return nil, "", err
	}
	row, err := s.q.CreateOAuthClient(ctx, db.CreateOAuthClientParams{
		Kind:         "platform",
		ClientID:     "oga9_plt_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		HashedSecret: hash,
		RedirectUris: in.RedirectURIs,
		Name:         in.Name,
		Trusted:      in.Trusted,
	})
	if err != nil {
		return nil, "", err
	}
	return &row, secret, nil
}

func (s *Service) ListOAuthClients(ctx context.Context) ([]db.OauthClients, error) {
	return s.q.ListOAuthClients(ctx)
}

func (s *Service) UpdateOAuthClient(ctx context.Context, id uuid.UUID, in OAuthClientIn) error {
	return s.q.UpdateOAuthClient(ctx, db.UpdateOAuthClientParams{
		ID: toPg(id), Name: in.Name, RedirectUris: in.RedirectURIs, Trusted: in.Trusted})
}

// SetOAuthClientStatus disables/re-enables a client; disabling also
// revokes its live grants (a disabled client must die everywhere).
func (s *Service) SetOAuthClientStatus(ctx context.Context, id uuid.UUID, status string) error {
	if status != "active" && status != "disabled" {
		return ErrBadRedirectURI
	}
	c, err := s.q.GetOAuthClient(ctx, toPg(id))
	if err != nil {
		return ErrClientNotFound
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if err := q.SetOAuthClientStatus(ctx, db.SetOAuthClientStatusParams{
			ID: c.ID, Status: status}); err != nil {
			return err
		}
		if status == "disabled" {
			return q.RevokeClientGrants(ctx, c.ClientID)
		}
		return nil
	})
}

// ---------- authorization flow ----------

func (s *Service) activeClient(ctx context.Context, clientID string) (*db.OauthClients, error) {
	c, err := s.q.GetOAuthClientByClientID(ctx, clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrClientNotFound
	}
	if err != nil {
		return nil, err
	}
	if c.Status != "active" || c.RevokedAt.Valid {
		return nil, ErrClientDisabled
	}
	return &c, nil
}

func (s *Service) clientSecretOK(c *db.OauthClients, secret string) bool {
	return secret != "" && CheckPassword(c.HashedSecret, secret)
}

func redirectRegistered(c *db.OauthClients, uri string) bool {
	for _, u := range c.RedirectUris {
		if u == uri {
			return true
		}
	}
	return false
}

// Authorize mints a single-use authorization code for an authenticated
// user. Connector clients additionally require membership in the client's
// org (access_denied) — platform clients carry no org context.
func (s *Service) Authorize(ctx context.Context, userID uuid.UUID, clientID, redirectURI, challenge, method string) (string, error) {
	c, err := s.activeClient(ctx, clientID)
	if err != nil {
		return "", err
	}
	if !redirectRegistered(c, redirectURI) {
		return "", ErrBadRedirectURI
	}
	if challenge == "" || (method != "" && method != "S256") {
		return "", ErrBadPKCE
	}
	if c.Kind == "connector" {
		if _, err := s.q.GetMembership(ctx, db.GetMembershipParams{
			OrganizationID: c.OrganizationID, UserID: toPg(userID)}); err != nil {
			return "", ErrAccessDenied
		}
	}
	codeBytes := make([]byte, 32)
	if _, err := rand.Read(codeBytes); err != nil {
		return "", err
	}
	code := hex.EncodeToString(codeBytes)
	return code, s.q.CreateAuthorizationCode(ctx, db.CreateAuthorizationCodeParams{
		Code: code, ClientID: c.ClientID, UserID: toPg(userID),
		OrganizationID: c.OrganizationID, RedirectUri: redirectURI, CodeChallenge: challenge})
}

type TokenResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// ExchangeCode redeems an authorization code (grant_type
// authorization_code): verifies client secret, code freshness + redirect
// parity, and PKCE S256; consumes the code atomically (consumed_at CAS
// makes replay a no-op), then mints session + grant.
func (s *Service) ExchangeCode(ctx context.Context, clientID, secret, code, redirectURI, verifier, ip, ua string) (*TokenResult, error) {
	c, err := s.activeClient(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if !s.clientSecretOK(c, secret) {
		return nil, ErrBadClientSecret
	}
	row, err := s.q.ConsumeAuthorizationCode(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCodeInvalid
	}
	if err != nil {
		return nil, err
	}
	if row.ClientID != clientID || row.RedirectUri != redirectURI ||
		row.ExpiresAt.Time.Before(time.Now()) {
		return nil, ErrCodeInvalid
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != row.CodeChallenge {
		return nil, ErrBadPKCE
	}
	refresh, err := s.mintGrant(ctx, c, fromPg(row.UserID), row.OrganizationID, ip, ua)
	if err != nil {
		return nil, err
	}
	return refresh, nil
}

// mintGrant creates the §02 session + oauth_grants row; on grant-insert
// failure the session is revoked so nothing dangles.
func (s *Service) mintGrant(ctx context.Context, c *db.OauthClients, userID uuid.UUID, orgID pgtype.UUID, ip, ua string) (*TokenResult, error) {
	token, sessionID, err := s.createSessionTTL(ctx, userID,
		"oauth:"+c.Name, ip, ua, "oauth:"+c.ClientID, s.tokenTTL, uuid.Nil)
	if err != nil {
		return nil, err
	}
	refreshBytes := make([]byte, 32)
	if _, err := rand.Read(refreshBytes); err != nil {
		return nil, err
	}
	refresh := hex.EncodeToString(refreshBytes)
	_, err = s.q.CreateOAuthGrant(ctx, db.CreateOAuthGrantParams{
		ClientID: c.ClientID, UserID: toPg(userID), OrganizationID: orgID,
		SessionID:        toPg(sessionID),
		RefreshTokenHash: pgText(hashToken(refresh)),
		RefreshExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(refreshGrantTTL), Valid: true},
	})
	if err != nil {
		_, _ = s.q.RevokeSessionByID(ctx, db.RevokeSessionByIDParams{ID: toPg(sessionID), UserID: toPg(userID)})
		return nil, err
	}
	return &TokenResult{AccessToken: token, RefreshToken: refresh,
		ExpiresIn: int64(s.tokenTTL.Seconds()), TokenType: "Bearer"}, nil
}

// RefreshGrant (grant_type refresh_token): verifies client + refresh
// hash, rotates it one-time, revokes the superseded session, mints a new
// session under the same grant.
func (s *Service) RefreshGrant(ctx context.Context, clientID, secret, refreshToken, ip, ua string) (*TokenResult, error) {
	c, err := s.activeClient(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if !s.clientSecretOK(c, secret) {
		return nil, ErrBadClientSecret
	}
	g, err := s.q.GetOAuthGrantByRefreshHash(ctx, db.GetOAuthGrantByRefreshHashParams{
		ClientID: clientID, RefreshTokenHash: pgText(hashToken(refreshToken))})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGrantInvalid
	}
	if err != nil {
		return nil, err
	}
	if g.RefreshExpiresAt.Valid && g.RefreshExpiresAt.Time.Before(time.Now()) {
		return nil, ErrGrantInvalid
	}
	userID := fromPg(g.UserID)
	token, sessionID, err := s.createSessionTTL(ctx, userID,
		"oauth:"+c.Name, ip, ua, "oauth:"+c.ClientID, s.tokenTTL, uuid.Nil)
	if err != nil {
		return nil, err
	}
	newRefreshBytes := make([]byte, 32)
	if _, err := rand.Read(newRefreshBytes); err != nil {
		return nil, err
	}
	newRefresh := hex.EncodeToString(newRefreshBytes)
	var oldSession pgtype.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		oldSession = g.SessionID
		return q.RotateOAuthGrant(ctx, db.RotateOAuthGrantParams{
			ID: g.ID, SessionID: toPg(sessionID), RefreshTokenHash: pgText(hashToken(newRefresh)),
			RefreshExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(refreshGrantTTL), Valid: true}})
	})
	if err != nil {
		return nil, err
	}
	if oldSession.Valid {
		if jti, err := s.q.RevokeSessionByID(ctx, db.RevokeSessionByIDParams{
			ID: oldSession, UserID: g.UserID}); err == nil {
			s.denySession(ctx, fromPg(jti))
		}
	}
	return &TokenResult{AccessToken: token, RefreshToken: newRefresh,
		ExpiresIn: int64(s.tokenTTL.Seconds()), TokenType: "Bearer"}, nil
}

// ---------- userinfo + revoke ----------

// UserinfoClaims resolves the caller's session → grant → claims.
// Connector clients get org-scoped claims; platform clients get global
// claims only (ADR-028).
func (s *Service) UserinfoClaims(ctx context.Context, jti uuid.UUID) (map[string]any, error) {
	sess, err := s.q.GetSessionByJTI(ctx, toPg(jti))
	if err != nil {
		return nil, ErrGrantInvalid
	}
	g, err := s.q.GetOAuthGrantBySession(ctx, sess.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGrantInvalid
	}
	if err != nil {
		return nil, err
	}
	u, err := s.q.GetUserByID(ctx, g.UserID)
	if err != nil {
		return nil, err
	}
	claims := map[string]any{
		"sub":            fromPg(g.UserID).String(),
		"email":          u.Email,
		"name":           u.DisplayName,
		"email_verified": u.IsVerified,
	}
	c, err := s.q.GetOAuthClientByClientID(ctx, g.ClientID)
	if err == nil && c.Kind == "connector" && g.OrganizationID.Valid {
		if m, err := s.q.GetMembership(ctx, db.GetMembershipParams{
			OrganizationID: g.OrganizationID, UserID: g.UserID}); err == nil {
			claims["oga9_org_id"] = fromPg(g.OrganizationID).String()
			claims["oga9_membership_id"] = fromPg(m.ID).String()
		}
	}
	return claims, nil
}

// RevokeSessionGrant revokes the grant bound to the caller's session and
// denylists the session itself (§02 machinery, unchanged).
func (s *Service) RevokeSessionGrant(ctx context.Context, jti uuid.UUID) error {
	sess, err := s.q.GetSessionByJTI(ctx, toPg(jti))
	if err != nil {
		return ErrGrantInvalid
	}
	g, err := s.q.GetOAuthGrantBySession(ctx, sess.ID)
	if err != nil {
		return ErrGrantInvalid
	}
	if err := s.q.RevokeOAuthGrant(ctx, g.ID); err != nil {
		return err
	}
	if _, err := s.q.RevokeSessionByJTI(ctx, toPg(jti)); err == nil {
		s.denySession(ctx, jti)
	}
	return nil
}
func (s *Service) OAuthClient(ctx context.Context, id uuid.UUID) (*db.OauthClients, error) {
	c, err := s.q.GetOAuthClient(ctx, toPg(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrClientNotFound
	}
	return &c, err
}

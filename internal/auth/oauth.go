// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md, ADR-021
// Generic OAuth2/OIDC authorization-code flow. A provider activates when its
// OGA9_OAUTH_{NAME}_CLIENT_ID+CLIENT_SECRET env vars are set — otherwise the
// route returns provider_not_configured.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jedafe/oga9/internal/db"
	valkey "github.com/valkey-io/valkey-go"
)

type ProviderConfig struct {
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	UserinfoURL  string
}

var ErrProviderNotConfigured = errors.New("provider_not_configured")
var ErrOAuthState = errors.New("oauth_state_invalid")

var oauthHTTP = &http.Client{Timeout: 10 * time.Second}

// StartOAuth stores a single-use state nonce (oga9:oauth:state:{state}) and
// returns the provider authorize URL.
func StartOAuth(ctx context.Context, vk valkey.Client, p ProviderConfig, redirectURI string) (string, error) {
	state := uuid.NewString()
	err := vk.Do(ctx, vk.B().Set().Key("oga9:oauth:state:"+state).Value("1").Nx().ExSeconds(600).Build()).Error()
	if err != nil {
		return "", err
	}
	q := url.Values{
		"client_id":     {p.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"state":         {state},
		"scope":         {"openid email profile"},
	}
	return p.AuthURL + "?" + q.Encode(), nil
}

type oidcUserinfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	EmailVerified bool   `json:"email_verified"`
}

// OAuthCallback consumes state, exchanges code for tokens, fetches userinfo,
// finds-or-provisions the user, links oauth_providers + japaconnect_user_id,
// then runs the standard post-credential gate (status, verified, MFA).
func (s *Service) OAuthCallback(ctx context.Context, state, code, redirectURI string, p ProviderConfig, provider, deviceLabel, ip, ua string) (*LoginResult, error) {
	deleted, err := s.vk.Do(ctx, s.vk.B().Del().Key("oga9:oauth:state:"+state).Build()).AsInt64()
	if err != nil || deleted != 1 {
		return nil, ErrOAuthState
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", p.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("oauth token exchange failed: %s", tok.Error)
	}

	ureq, _ := http.NewRequestWithContext(ctx, "GET", p.UserinfoURL, nil)
	ureq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uresp, err := oauthHTTP.Do(ureq)
	if err != nil {
		return nil, err
	}
	defer uresp.Body.Close()
	if uresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth userinfo returned %d", uresp.StatusCode)
	}
	var info oidcUserinfo
	if err := json.NewDecoder(uresp.Body).Decode(&info); err != nil || info.Sub == "" {
		return nil, errors.New("oauth userinfo failed")
	}

	userID, err := s.findOrProvisionOAuthUser(ctx, provider, info)
	if err != nil {
		return nil, err
	}

	// Same gates as password login: suspended and unverified accounts get no
	// session, whatever the provider asserted.
	state2, err := s.q.GetUserAuthState(ctx, toPg(userID))
	if err != nil {
		return nil, err
	}
	if state2.Status != "active" {
		return nil, ErrUserSuspended
	}
	if !state2.IsVerified {
		return nil, ErrEmailNotVerified
	}
	return s.finishLogin(ctx, userID, state2.MfaSecret, "", deviceLabel, ip, ua, provider)
}

func (s *Service) findOrProvisionOAuthUser(ctx context.Context, provider string, info oidcUserinfo) (uuid.UUID, error) {
	userID, err := s.userByOAuthSubject(ctx, provider, info.Sub)
	if err == nil {
		return userID, s.linkJapaID(ctx, userID, provider, info.Sub)
	}
	if err != pgx.ErrNoRows {
		return uuid.Nil, err
	}

	// link by email only when the provider VERIFIED the address — otherwise
	// an unverified provider account could claim an existing OGA9 user.
	if info.Email != "" && info.EmailVerified {
		if id, e := s.UserIDByEmail(ctx, info.Email); e == nil {
			userID = uuid.MustParse(id)
		}
	}
	if userID == uuid.Nil {
		// new account: provision + emit + link in one tx (Rule 12)
		display := info.Name
		if display == "" {
			display = info.Email
		}
		err = s.withTx(ctx, func(q *db.Queries) error {
			id, e := q.CreateOAuthUser(ctx, db.CreateOAuthUserParams{
				Email: info.Email, DisplayName: display, IsVerified: info.EmailVerified,
			})
			if e != nil {
				return e
			}
			userID = fromPg(id)
			if e := emit(ctx, q, "UserRegistered", "user", userID, userID, map[string]any{"provider": provider}); e != nil {
				return e
			}
			return q.LinkOAuthProvider(ctx, db.LinkOAuthProviderParams{
				UserID: toPg(userID), Provider: provider, ProviderSubject: info.Sub,
			})
		})
		if err != nil {
			// concurrent first login for the same email — the other tx won;
			// re-run the lookup instead of surfacing a 500.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				if id, e := s.userByOAuthSubject(ctx, provider, info.Sub); e == nil {
					return id, nil
				}
				if id, e := s.UserIDByEmail(ctx, info.Email); e == nil {
					userID = uuid.MustParse(id)
					return userID, s.linkAndJapa(ctx, userID, provider, info.Sub)
				}
			}
			return uuid.Nil, err
		}
		return userID, s.linkJapaID(ctx, userID, provider, info.Sub)
	}
	return userID, s.linkAndJapa(ctx, userID, provider, info.Sub)
}

// linkAndJapa records the provider link and — for japaconnect — the ADR-021
// account link column (users.japaconnect_user_id = provider sub when it is a
// UUID).
func (s *Service) linkAndJapa(ctx context.Context, userID uuid.UUID, provider, sub string) error {
	if err := s.LinkOAuthProvider(ctx, userID, provider, sub); err != nil {
		return err
	}
	return s.linkJapaID(ctx, userID, provider, sub)
}

func (s *Service) linkJapaID(ctx context.Context, userID uuid.UUID, provider, sub string) error {
	if provider != "japaconnect" {
		return nil
	}
	jid, err := uuid.Parse(sub)
	if err != nil {
		return nil // non-uuid sub — oauth_providers link still recorded
	}
	return s.q.SetJapaconnectUserID(ctx, db.SetJapaconnectUserIDParams{
		UserID: toPg(userID), JapaconnectUserID: toPg(jid),
	})
}

// Ref: oga9-engineering/sections/23-integrations.md — connectors, signed
// webhook intake (verify→persist→enqueue→200, Rule 9), idempotent sync
// via external_event_map (Rule 27), api_keys, ICS feed, JapaConnect.
package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrBadValue     = errors.New("bad value")
	ErrBadSignature = errors.New("invalid_signature")
	ErrRevoked      = errors.New("api_key_revoked")
)

var connectorKinds = map[string]bool{"paystack": true, "flutterwave": true,
	"smtp": true, "ics": true, "japaconnect": true}

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	// seams (Rule 26): secretFn resolves credential_ref → the webhook
	// secret (secrets manager); applyFn lands locator joins as membership
	// applications; calFn supplies ICS events from activities.
	secretFn func(ctx context.Context, ref string) (string, error)
	// postFn is the outbound transport for sync jobs — injectable for
	// tests; defaults to a real http.Client POST in main.
	postFn  func(ctx context.Context, url, authHeader string, payload []byte) error
	applyFn func(ctx context.Context, slug, email, phone, japaSubject string, answers json.RawMessage) (uuid.UUID, error)
	calFn   func(ctx context.Context, oc *tenant.OrgCtx) (string, error)
	// oga9:ics:{org_id} feed cache (Rule 4 — catalog entry in §23).
	feedGet func(ctx context.Context, orgID uuid.UUID) (string, error)
	feedSet func(ctx context.Context, orgID uuid.UUID, feed string) error
	// self-hosted opt-in (OGA9_CONNECTOR_ALLOW_PRIVATE_TARGETS): locator_url
	// may be http/private/loopback for in-house connectors.
	allowPrivate bool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

func (s *Service) SetSecret(fn func(context.Context, string) (string, error))     { s.secretFn = fn }
func (s *Service) SetPost(fn func(context.Context, string, string, []byte) error) { s.postFn = fn }
func (s *Service) SetApply(fn func(context.Context, string, string, string, string, json.RawMessage) (uuid.UUID, error)) {
	s.applyFn = fn
}
func (s *Service) SetCalendar(fn func(context.Context, *tenant.OrgCtx) (string, error)) {
	s.calFn = fn
}
func (s *Service) SetAllowPrivateTargets(v bool) { s.allowPrivate = v }

func (s *Service) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------- connectors ----------

// Configure — non-secret config lands in config JSONB; the secret stays
// behind credential_ref (Rule: secrets never in DB).
// validateOutboundURL — delivery targets are admin-supplied; require https
// and a public host so the push path can't be turned into an internal
// probe or a secret-exfil sink (credential_ref rides as the bearer).
// allowPrivate (operator opt-in, self-hosted) relaxes to http/https with
// a non-empty host — in-house connectors live on private nets there.
func validateOutboundURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ErrBadValue
	}
	if allowPrivate {
		if u.Scheme != "https" && u.Scheme != "http" {
			return ErrBadValue
		}
		return nil
	}
	if u.Scheme != "https" {
		return ErrBadValue
	}
	h := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || !ip.IsGlobalUnicast() {
			return ErrBadValue
		}
		return nil
	}
	if !strings.Contains(h, ".") {
		return ErrBadValue // bare/local hostnames can't be delivery targets
	}
	return nil
}

func (s *Service) Configure(ctx context.Context, oc *tenant.OrgCtx, kind string,
	config json.RawMessage, credentialRef string) (uuid.UUID, error) {
	if !connectorKinds[kind] {
		return uuid.Nil, ErrBadValue
	}
	var probe struct {
		LocatorURL string `json:"locator_url"`
	}
	_ = json.Unmarshal(config, &probe)
	if probe.LocatorURL != "" && validateOutboundURL(probe.LocatorURL, s.allowPrivate) != nil {
		return uuid.Nil, ErrBadValue
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		cid, err := q.CreateConnector(ctx, db.CreateConnectorParams{
			OrgID: toPg(oc.OrgID), Kind: kind, Config: config, CredentialRef: pgText(credentialRef)})
		if err != nil {
			return err
		}
		id = fromPg(cid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ConnectorConfigured",
			"connector", id, oc.UserID, false,
			map[string]any{"connector_id": id, "kind": kind})
	})
	return id, err
}

func (s *Service) ListConnectors(ctx context.Context, oc *tenant.OrgCtx) ([]db.Connectors, error) {
	return s.q.ListConnectors(ctx, toPg(oc.OrgID))
}

func (s *Service) SetEnabled(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID, enabled bool) error {
	n, err := s.q.SetConnectorEnabled(ctx, db.SetConnectorEnabledParams{
		ID: toPg(cid), OrganizationID: toPg(oc.OrgID), Enabled: enabled})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- webhooks (Rule 9) ----------

// Intake — verify signature → persist raw receipt → caller enqueues the
// processing task and returns 200 fast. A bad signature is recorded AND
// rejected (spec test 2).
func (s *Service) Intake(ctx context.Context, provider string, raw []byte, signature string) (uuid.UUID, bool, error) {
	if !json.Valid(raw) {
		return uuid.Nil, false, ErrBadValue // malformed payload — no receipt
	}
	conns, _ := s.q.ConnectorsByKind(ctx, provider)
	sigOK := false
	var orgID uuid.UUID
	if s.secretFn != nil {
		for _, conn := range conns {
			if !conn.CredentialRef.Valid {
				continue
			}
			secret, err := s.secretFn(ctx, conn.CredentialRef.String)
			if err == nil && secret != "" && verifySignature(provider, secret, raw, signature) {
				sigOK = true
				orgID = fromPg(conn.OrganizationID) // the verifying connector attributes the org
				break
			}
		}
	}
	rid, err := s.q.InsertWebhookReceipt(ctx, db.InsertWebhookReceiptParams{
		Provider: provider, Raw: raw, SignatureOk: sigOK, OrganizationID: toPg(orgID)})
	if err != nil {
		return uuid.Nil, false, err
	}
	if !sigOK {
		return fromPg(rid), false, ErrBadSignature
	}
	return fromPg(rid), true, nil
}

// verifySignature — provider-specific schemes. paystack: HMAC-SHA512 hex
// of the raw body (x-paystack-signature); flutterwave: shared-hash compare
// (verif-hash header); generic: HMAC-SHA256 hex.
func verifySignature(provider, secret string, raw []byte, sig string) bool {
	if sig == "" {
		return false
	}
	switch provider {
	case "paystack":
		mac := hmac.New(sha512.New, []byte(secret))
		mac.Write(raw)
		return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig))
	case "flutterwave":
		return hmac.Equal([]byte(secret), []byte(sig))
	default:
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(raw)
		return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig))
	}
}

// ProcessReceipt — integrations:webhook_process: the recorded raw event
// emits ExternalEventReceived for downstream consumers; receipt marked.
func (s *Service) ProcessReceipt(ctx context.Context, receiptID uuid.UUID) error {
	var orgID, tid uuid.UUID
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.MarkReceiptProcessed(ctx, toPg(receiptID))
		if err != nil {
			return err
		}
		if n == 0 {
			return nil // already processed — idempotent
		}
		// attribute the event to the org the verifying connector proved
		// at intake (nil org = platform-scoped provider event).
		if r, err := q.WebhookReceipt(ctx, toPg(receiptID)); err == nil {
			orgID = fromPg(r.OrganizationID)
			if orgID != uuid.Nil {
				if t, err := q.OrgTenantID(ctx, r.OrganizationID); err == nil {
					tid = fromPg(t)
				}
			}
		}
		return events.Emit(ctx, q, tid, orgID, "ExternalEventReceived",
			"webhook_receipt", receiptID, uuid.Nil, false,
			map[string]any{"receipt_id": receiptID, "org_id": orgID})
	})
}

// ---------- japa sync (ADR-021) ----------

// JapaPushConsumer — OrganizationPublished/LocationUpdated: dedupe on
// the external_event_map (Rule 27), then create a push sync_job for the
// japaconnect connector. Only is_public_listed orgs sync.
func (s *Service) JapaPushConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	if e.Type != "OrganizationPublished" && e.Type != "OrganizationLocationUpdated" {
		return nil
	}
	orgStr, _ := e.Payload["org_id"].(string)
	orgID, err := uuid.Parse(orgStr)
	if err != nil || orgID == uuid.Nil {
		orgID = e.OrganizationID
	}
	if orgID == uuid.Nil {
		return nil
	}
	mapped, err := q.InternalMapped(ctx, db.InternalMappedParams{
		InternalType: "organization", InternalID: toPg(orgID), ExternalType: "japaconnect:locator"})
	if err != nil {
		return err
	}
	if mapped && e.Type == "OrganizationPublished" {
		return nil // test 1: re-push of an already-synced org is a dedupe hit
	}
	conn, err := q.ConnectorByKind(ctx, db.ConnectorByKindParams{Kind: "japaconnect", OrgID: toPg(orgID)})
	if err != nil {
		return nil // no connector configured — pull fallback covers it
	}
	detail, _ := json.Marshal(map[string]any{"org_id": orgID, "trigger": e.Type})
	jid, err := q.CreateSyncJob(ctx, db.CreateSyncJobParams{
		ConnectorID: conn.ID, Direction: "push", Detail: detail})
	if err != nil {
		return err
	}
	if !mapped {
		if err := q.MapExternal(ctx, db.MapExternalParams{
			InternalType: "organization", InternalID: toPg(orgID),
			ExternalType: "japaconnect:locator", ExternalID: "oga9_org_id:" + orgID.String()}); err != nil {
			return err
		}
	}
	return events.Emit(ctx, q, e.TenantID, orgID, "OrganizationSynced",
		"sync_job", fromPg(jid), uuid.Nil, false,
		map[string]any{"sync_job_id": fromPg(jid), "org_id": orgID})
}

// Reconcile — integrations:japa_sync_reconcile (daily): listed orgs
// missing a locator mapping get a synthetic push via the consumer path.
func (s *Service) Reconcile(ctx context.Context) (int, error) {
	// Retention: raw delivery payloads don't outlive their debugging
	// window — unsigned/unbounded intake rows can't grow the table forever.
	if err := s.q.PruneProcessedReceipts(ctx); err != nil {
		return 0, err
	}
	orgs, err := s.q.PublicOrgNeedsSync(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	for _, oid := range orgs {
		err := s.withTx(ctx, func(q *db.Queries) error {
			return s.JapaPushConsumer(ctx, q, events.Event{
				Type:           "OrganizationPublished",
				OrganizationID: fromPg(oid),
				Payload:        map[string]any{"org_id": fromPg(oid).String()},
			})
		})
		if err == nil {
			n++
		}
	}
	return n, nil
}

// ---------- locator join (ADR-021) ----------

// LocatorJoin — POST /directory/orgs/{slug}/join: a japa locator referral
// lands as a membership application (existing intake) + the sourced
// MembershipRequestReceived event carrying the japa identity.
func (s *Service) LocatorJoin(ctx context.Context, slug, email, phone, japaSubject, referralToken string,
	answers json.RawMessage) (uuid.UUID, error) {
	if s.applyFn == nil {
		return uuid.Nil, ErrBadValue
	}
	orgs, err := s.q.PublicOrgBySlug(ctx, slug)
	if err != nil || len(orgs) == 0 {
		return uuid.Nil, ErrNotFound
	}
	org := orgs[0]
	extra := answers
	if len(extra) == 0 {
		extra = json.RawMessage("{}")
	}
	subject, err := s.verifyReferral(ctx, fromPg(org.ID), japaSubject, referralToken)
	if err != nil {
		return uuid.Nil, err
	}
	// the verified subject ("" for plain joins) lands on the application
	// row — the durable §23 link, not event-payload only.
	appID, err := s.applyFn(ctx, slug, email, phone, subject, extra)
	if err != nil {
		return uuid.Nil, err
	}
	payload := map[string]any{"application_id": appID, "org_id": fromPg(org.ID), "source": "japaconnect"}
	if subject != "" {
		payload["japa_subject"] = subject // only when the token proved it
	}
	var tid uuid.UUID
	if t, err := s.q.OrgTenantID(ctx, org.ID); err == nil {
		tid = fromPg(t)
	}
	err = s.withTx(ctx, func(q *db.Queries) error {
		return events.Emit(ctx, q, tid, fromPg(org.ID), "MembershipRequestReceived",
			"membership_application", appID, uuid.Nil, true, payload)
	})
	return appID, err
}

// verifyReferral — §23 referral-token contract:
// `base64url(json).base64url(hmac-sha256)`; the JSON carries
// japa_subject + exp, signed with the japaconnect connector's
// credential_ref secret. A claimed japa_subject without a proving token
// is REJECTED (never trust request-supplied identity, Rule 24 pattern);
// no subject at all → a plain application.
func (s *Service) verifyReferral(ctx context.Context, orgID uuid.UUID, japaSubject, token string) (string, error) {
	if token == "" {
		if japaSubject != "" {
			return "", ErrBadSignature // claimed identity without proof
		}
		return "", nil // plain application — no japa link
	}
	dot := strings.LastIndexByte(token, '.')
	if dot <= 0 {
		return "", ErrBadSignature
	}
	payloadB64, sigB64 := token[:dot], token[dot+1:]
	payloadRaw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return "", ErrBadSignature
	}
	var claims struct {
		Subject string `json:"japa_subject"`
		Exp     int64  `json:"exp"`
	}
	if err := json.Unmarshal(payloadRaw, &claims); err != nil || claims.Subject == "" {
		return "", ErrBadSignature
	}
	if claims.Exp <= 0 || time.Now().Unix() > claims.Exp {
		return "", ErrBadSignature // missing or expired referral
	}
	// §23: the signature verifies against THE TARGET ORG's japaconnect
	// connector secret — a token minted for another org must not cross.
	conn, err := s.q.ConnectorByKind(ctx, db.ConnectorByKindParams{Kind: "japaconnect", OrgID: toPg(orgID)})
	if err != nil {
		return "", ErrBadSignature // no org connector → nothing to verify against
	}
	conns := []db.Connectors{conn}
	for _, conn := range conns {
		if !conn.CredentialRef.Valid || s.secretFn == nil {
			continue
		}
		secret, err := s.secretFn(ctx, conn.CredentialRef.String)
		if err != nil || secret == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(payloadB64))
		if hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), []byte(sigB64)) {
			return claims.Subject, nil // token proved the subject
		}
	}
	return "", ErrBadSignature
}

// ---------- api keys ----------

// IssueKey — user-scoped key (tenant NULL for /me self-management); the
// raw secret is returned ONCE, only the SHA-256 hash is stored.
func (s *Service) IssueKey(ctx context.Context, userID uuid.UUID,
	scopes []string) (keyID uuid.UUID, rawKey string, err error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return uuid.Nil, "", err
	}
	raw := "oga9_" + hex.EncodeToString(buf)
	hash := sha256.Sum256([]byte(raw))
	id, err := s.q.InsertAPIKey(ctx, db.InsertAPIKeyParams{
		UserID: toPg(userID), Prefix: raw[:12],
		HashedKey: hex.EncodeToString(hash[:]), Scopes: scopes, RateTier: "standard"})
	return fromPg(id), raw, err
}

func (s *Service) ListKeys(ctx context.Context, userID uuid.UUID) ([]db.ListAPIKeysForUserRow, error) {
	return s.q.ListAPIKeysForUser(ctx, toPg(userID))
}

// Revoke — immediate: the next auth check hits revoked_at (test 4).
func (s *Service) Revoke(ctx context.Context, userID, keyID uuid.UUID) error {
	n, err := s.q.RevokeAPIKeyForUser(ctx, db.RevokeAPIKeyForUserParams{ID: toPg(keyID), UserID: toPg(userID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Authenticate — X-API-Key bearer → tenant/user identity. Revoked or
// unknown keys fail closed (immediate 401 semantics).
func (s *Service) Authenticate(ctx context.Context, rawKey string) (db.ApiKeys, error) {
	var k db.ApiKeys
	if len(rawKey) < 16 {
		return k, ErrRevoked
	}
	hash := sha256.Sum256([]byte(rawKey))
	k, err := s.q.APIKeyByHash(ctx, hex.EncodeToString(hash[:]))
	if err != nil || k.RevokedAt.Valid {
		return k, ErrRevoked
	}
	return k, nil
}

// ---------- ICS ----------

// Calendar — GET /orgs/{id}/calendar.ics via the activities seam;
// serves the oga9:ics:{org_id} cache when warm, then re-renders + stores.
func (s *Service) Calendar(ctx context.Context, oc *tenant.OrgCtx) (string, error) {
	if s.feedGet != nil {
		if feed, err := s.feedGet(ctx, oc.OrgID); err == nil && feed != "" {
			return feed, nil
		}
	}
	if s.calFn == nil {
		return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//OGA9//EN\r\nEND:VCALENDAR\r\n", nil
	}
	feed, err := s.calFn(ctx, oc)
	if err != nil {
		return "", err
	}
	if s.feedSet != nil {
		_ = s.feedSet(ctx, oc.OrgID, feed)
	}
	return feed, nil
}

// IcsFeedSweep — integrations:ics_feed (low): reserved — ICS is rendered
// on demand; the sweep is a registered no-op for connector health-checks.
// ProcessQueuedReceipts — the periodic form of webhook_process: drains
// receipts that arrived while the sweep wasn't running.
func (s *Service) ProcessQueuedReceipts(ctx context.Context) error {
	rows, err := s.q.UnprocessedReceipts(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := s.ProcessReceipt(ctx, fromPg(r.ID)); err != nil {
			return err
		}
	}
	return nil
}

// PushSyncJobs — integrations:japa_sync_push: drain queued push jobs.
// Each job POSTs its payload to the connector's config.locator_url
// (ADR-021 JEOS ingestion endpoint). credential_ref supplies the bearer
// token. A connector without locator_url records 'done' — the inbound
// pull fallback (directory endpoint) is the delivery path in that case.
func (s *Service) PushSyncJobs(ctx context.Context) (int, error) {
	rows, err := s.q.QueuedSyncJobs(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	for _, j := range rows {
		// CAS claim — a concurrent drain (two workers on the same queue)
		// must not double-deliver; the loser sees 0 rows and skips.
		claimed, err := s.q.ClaimSyncJob(ctx, j.ID)
		if err != nil {
			return n, err
		}
		if claimed == 0 {
			continue
		}
		if err := s.deliverJob(ctx, j); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// deliverJob performs one push: POST the job detail to locator_url and
// record the outcome + SyncCompleted/SyncFailed (catalog events).
func (s *Service) deliverJob(ctx context.Context, j db.SyncJobs) error {
	conn, err := s.q.ConnectorByID(ctx, j.ConnectorID)
	if err != nil {
		return err
	}
	if !conn.Enabled {
		return nil // disabled mid-flight — leave queued for operator
	}
	var cfg struct {
		LocatorURL string `json:"locator_url"`
	}
	_ = json.Unmarshal(conn.Config, &cfg)

	state, note := "done", ""
	switch {
	case cfg.LocatorURL == "":
		note = "no locator_url configured — inbound pull fallback covers delivery (ADR-021)"
	case validateOutboundURL(cfg.LocatorURL, s.allowPrivate) != nil:
		// re-validate at delivery — rows pre-dating the configure-time
		// check must never reach the transport either.
		state, note = "failed", "locator_url rejected: not a public https endpoint"
	case s.postFn == nil:
		note = "no transport configured — job recorded"
	case j.Direction == "push":
		auth := ""
		if conn.CredentialRef.Valid && s.secretFn != nil {
			if sec, err := s.secretFn(ctx, conn.CredentialRef.String); err == nil {
				auth = "Bearer " + sec
			}
		}
		payload := j.Detail
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		if err := s.postFn(ctx, cfg.LocatorURL, auth, payload); err != nil {
			state, note = "failed", err.Error()
		}
	}
	detail, _ := json.Marshal(map[string]any{"result": note})
	if _, err := s.q.FinishSyncJob(ctx, db.FinishSyncJobParams{
		ID: j.ID, State: state, Detail: detail}); err != nil {
		return err
	}
	if state == "failed" {
		return s.withTx(ctx, func(q *db.Queries) error {
			orgID := fromPg(conn.OrganizationID)
			var tid uuid.UUID
			if orgID != uuid.Nil {
				if t, err := q.OrgTenantID(ctx, conn.OrganizationID); err == nil {
					tid = fromPg(t)
				}
			}
			return events.Emit(ctx, q, tid, orgID, "SyncFailed",
				"sync_job", fromPg(j.ID), uuid.Nil, false,
				map[string]any{"sync_job_id": fromPg(j.ID), "org_id": orgID, "error": note})
		})
	}
	return nil
}

// PublicOrg — the directory org detail read.
func (s *Service) PublicOrg(ctx context.Context, slug string) ([]db.PublicOrgBySlugRow, error) {
	if slug == "" {
		return nil, ErrBadValue
	}
	return s.q.PublicOrgBySlug(ctx, slug)
}

// ---------- tenant-scoped api keys (§23) ----------

// IssueTenantKey — control-plane issued key for the tenant itself
// (user_id NULL): automation/service integrations at the billing boundary.
// Same scope/rate-tier model as user keys; tenant_id set on the row.
func (s *Service) IssueTenantKey(ctx context.Context, tenantID uuid.UUID,
	scopes []string) (keyID uuid.UUID, rawKey string, err error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return uuid.Nil, "", err
	}
	raw := "oga9_" + hex.EncodeToString(buf)
	hash := sha256.Sum256([]byte(raw))
	id, err := s.q.InsertAPIKey(ctx, db.InsertAPIKeyParams{
		TenantID: toPg(tenantID), Prefix: raw[:12],
		HashedKey: hex.EncodeToString(hash[:]), Scopes: scopes, RateTier: "standard"})
	return fromPg(id), raw, err
}

// ListTenantKeys — prefix/scope/rate-tier only, never the hash.
func (s *Service) ListTenantKeys(ctx context.Context, tenantID uuid.UUID) ([]db.ListAPIKeysForTenantRow, error) {
	return s.q.ListAPIKeysForTenant(ctx, toPg(tenantID))
}

// RevokeTenantKey — CAS on revoked_at (Rule 27-safe).
func (s *Service) RevokeTenantKey(ctx context.Context, tenantID, keyID uuid.UUID) error {
	n, err := s.q.RevokeAPIKeyForTenant(ctx, db.RevokeAPIKeyForTenantParams{
		ID: toPg(keyID), TenantID: toPg(tenantID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- ics feed cache (§23) ----------

// SetFeedCache wires the oga9:ics:{org_id} feed cache (Rule 4 key in the
// catalog). get misses → regenerate; set overwrites with fresh bytes.
func (s *Service) SetFeedCache(getFn func(ctx context.Context, orgID uuid.UUID) (string, error),
	setFn func(ctx context.Context, orgID uuid.UUID, feed string) error) {
	s.feedGet, s.feedSet = getFn, setFn
}

// IcsFeedSweep — pre-warm feed caches for orgs with enabled ics connectors
// (the catalog task does real work now, not just a health check).
func (s *Service) IcsFeedSweep(ctx context.Context) error {
	if s.calFn == nil || s.feedSet == nil {
		return nil // calendar/cache unwired — nothing to warm
	}
	orgs, err := s.q.EnabledIcsOrgs(ctx)
	if err != nil {
		return err
	}
	for _, oid := range orgs {
		orgID := uuid.UUID(oid.Bytes)
		tid, err := s.q.OrgTenantID(ctx, toPg(orgID))
		if err != nil {
			continue
		}
		oc := &tenant.OrgCtx{OrgID: orgID, TenantID: fromPg(tid)}
		feed, err := s.calFn(ctx, oc)
		if err != nil || feed == "" {
			continue
		}
		if err := s.feedSet(ctx, orgID, feed); err != nil {
			return err
		}
	}
	return nil
}

// PublicOrgProfile — the portal landing payload (ROADMAP-MAIN 07.01):
// opted-in fields + branding + the current-term EXCO roster.
func (s *Service) PublicOrgProfile(ctx context.Context, slug string) (map[string]any, error) {
	if slug == "" {
		return nil, ErrBadValue
	}
	o, err := s.q.PublicOrgBySlugFull(ctx, slug)
	if err != nil {
		return nil, err
	}
	exco, err := s.q.PublicOrgExco(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": o.ID, "name": o.Name, "slug": o.Slug, "city": o.City,
		"country_code": o.CountryCode, "maturity_level": o.MaturityLevel,
		"description": o.Description, "member_count": o.MemberCount,
		"branding": map[string]any{
			"primary_color": o.PrimaryColor, "secondary_color": o.SecondaryColor,
			"motto": o.Motto, "founded_year": o.FoundedYear,
		},
		"exco": exco,
	}, nil
}

// ResolveHost maps a request host to a listed org — `{slug}.{base}` strips
// the base domain; anything else is a custom_domain lookup (§04/ADR-021).
// The public portal calls this to resolve its own host before rendering.
// ResolveHost → (slug, host_kind). host_kind tells the caller which surface
// the host represents: "admin" (admin.{slug}.{base} — org console),
// "subdomain" ({slug}.{base} portal), or "custom" (organizations.custom_domain).
// Platform surfaces (app/www/docs/api/base apex) are not org addresses.
var reservedSurfaces = map[string]bool{
	"app": true, "admin": true, "control": true, "api": true,
	"www": true, "docs": true,
}

func (s *Service) ResolveHost(ctx context.Context, host, baseDomain string) (string, string, error) {
	host = strings.ToLower(strings.Split(host, ":")[0])
	suffix := "." + strings.ToLower(baseDomain)
	if strings.HasSuffix(host, suffix) {
		head := strings.TrimSuffix(host, suffix)
		if head == "" || reservedSurfaces[head] {
			return "", "", ErrBadValue
		}
		if strings.HasPrefix(head, "admin.") {
			// admin.{slug}.{base} — the org console interface
			slug := strings.TrimPrefix(head, "admin.")
			if slug == "" || strings.Contains(slug, ".") || reservedSurfaces[slug] {
				return "", "", ErrBadValue
			}
			return slug, "admin", nil
		}
		if strings.Contains(head, ".") {
			return "", "", ErrBadValue // deeper nesting isn't an org address
		}
		return head, "subdomain", nil
	}
	if strings.HasPrefix(host, "www.") {
		host = host[4:] // www-alias resolves to the bare custom domain
	}
	r, err := s.q.OrgByCustomDomain(ctx, host)
	if err != nil {
		return "", "", err
	}
	return r, "custom", nil
}

package auth

import (
	"context"
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
	"github.com/pquerna/otp/totp"
	valkey "github.com/valkey-io/valkey-go"
)

var testKey = sha256.Sum256([]byte("test-data-key"))

// ---------- pure unit tests (no infra) ----------

func TestPasswordHashRoundtrip(t *testing.T) {
	h, err := HashPassword("s3cure-pw")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "s3cure-pw") {
		t.Fatal("roundtrip failed")
	}
	if CheckPassword(h, "wrong") {
		t.Fatal("wrong password accepted")
	}
}

func TestValidatePassword(t *testing.T) {
	for _, bad := range []string{"", "short1", "onlyletters", "12345678"} {
		if err := ValidatePassword(bad); err != ErrWeakPassword {
			t.Fatalf("%q should be weak", bad)
		}
	}
	if err := ValidatePassword("goodpass1"); err != nil {
		t.Fatal(err)
	}
}

func TestJWTRoundtrip(t *testing.T) {
	uid, jti := uuid.New(), uuid.New()
	tok, err := IssueToken("sec", uid, jti, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseToken("sec", tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != uid.String() || c.ID != jti.String() {
		t.Fatal("claims mismatch")
	}
	if _, err := ParseToken("other-secret", tok); err == nil {
		t.Fatal("wrong secret accepted")
	}
}

func TestActionTokenPurpose(t *testing.T) {
	tok, _ := SignActionToken("sec", "uid-1", "verify_email", time.Hour)
	if _, _, err := VerifyActionToken("sec", tok, "verify_email"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyActionToken("sec", tok, "password_reset"); err == nil {
		t.Fatal("cross-purpose token accepted")
	}
}

func TestBlobRoundtrip(t *testing.T) {
	ct, err := encryptBlob(testKey[:], []byte("JBSWY3DPEHPK3PXP"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := decryptBlob(testKey[:], ct)
	if err != nil || string(pt) != "JBSWY3DPEHPK3PXP" {
		t.Fatal("roundtrip failed")
	}
	other := sha256.Sum256([]byte("other-key"))
	if _, err := decryptBlob(other[:], ct); err == nil {
		t.Fatal("wrong key decrypted")
	}
}

// ---------- integration tests (need live stack) ----------

func testSvc(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	vk, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{envOr("OGA9_TEST_VALKEY_ADDR", "localhost:6379")}})
	if err != nil {
		t.Fatal(err)
	}
	return NewService(pool, vk, "test-secret", time.Hour, testKey[:])
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func verifiedUser(t *testing.T, s *Service, prefix string) (*User, string) {
	t.Helper()
	email := prefix + "-" + uuid.NewString()[:8] + "@oga9.test"
	u, vt, err := s.Register(context.Background(), email, "password1", "T User")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.VerifyEmail(context.Background(), vt); err != nil {
		t.Fatalf("verify: %v", err)
	}
	return u, email
}

func TestRegisterLoginLogoutFlow(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	u, email := verifiedUser(t, s, "it")

	// display-name form normalizes to bare address
	nu, _, err := s.Register(ctx, "Norm <n-"+uuid.NewString()[:8]+"@oga9.test>", "password1", "N")
	if err != nil {
		t.Fatalf("normalized register: %v", err)
	}
	if nu.Email == "" || nu.Email[0] != 'n' {
		t.Fatalf("expected bare addr, got %q", nu.Email)
	}

	// weak password rejected
	if _, _, err = s.Register(ctx, "w-"+uuid.NewString()[:8]+"@t.t", "weak", "x"); err != ErrWeakPassword {
		t.Fatal("weak password not rejected")
	}
	// duplicate email
	if _, _, err = s.Register(ctx, email, "password1", "x"); err != ErrEmailTaken {
		t.Fatalf("dup email: %v", err)
	}
	// bad login
	if _, err = s.Login(ctx, email, "wrongpw1", "", "", "127.0.0.1", "ua"); err != ErrBadCredentials {
		t.Fatalf("bad login: %v", err)
	}
	// good login
	res, err := s.Login(ctx, email, "password1", "", "dev", "127.0.0.1", "ua")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	claims, err := ParseToken("test-secret", res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveSession(ctx, uuid.MustParse(claims.ID)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// revoke
	rev, err := s.RevokeSession(ctx, u.ID, res.SessionID)
	if err != nil || !rev {
		t.Fatal("revoke")
	}
	if rev, _ := s.RevokeSession(ctx, u.ID, res.SessionID); rev {
		t.Fatal("double revoke should report false")
	}
	if _, err = s.ResolveSession(ctx, uuid.MustParse(claims.ID)); err == nil {
		t.Fatal("revoked session still resolves")
	}
}

func TestSuspendedUserSessionDies(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	u, email := verifiedUser(t, s, "sus")

	res, err := s.Login(ctx, email, "password1", "", "", "127.0.0.1", "ua")
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := ParseToken("test-secret", res.Token)
	if _, err := s.ResolveSession(ctx, uuid.MustParse(claims.ID)); err != nil {
		t.Fatal("session should resolve while active")
	}
	// suspend the account — the live session must stop resolving
	if _, err := s.pool.Exec(ctx, "UPDATE users SET status='suspended' WHERE id=$1", toPg(u.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveSession(ctx, uuid.MustParse(claims.ID)); err != ErrUserSuspended {
		t.Fatalf("expected user_suspended, got %v", err)
	}
	if _, err := s.Login(ctx, email, "password1", "", "", "127.0.0.1", "ua"); err != ErrUserSuspended {
		t.Fatalf("suspended login: %v", err)
	}
}

func TestMFALoginEnforcement(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	u, email := verifiedUser(t, s, "mfa")

	key, err := totp.Generate(totp.GenerateOpts{Issuer: "T", AccountName: email})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetupMFA(ctx, u.ID, key.Secret()); err != nil {
		t.Fatal(err)
	}
	// secret must be ciphertext at rest, not the base32 plaintext
	var raw []byte
	if err := s.pool.QueryRow(ctx, "SELECT mfa_secret FROM users WHERE id=$1", toPg(u.ID)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) == key.Secret() || len(raw) == 0 {
		t.Fatal("mfa_secret stored as plaintext")
	}

	// no code → mfa_required + gate token
	res, err := s.Login(ctx, email, "password1", "", "", "127.0.0.1", "ua")
	if err != ErrMFARequired || res == nil || res.MFAToken == "" {
		t.Fatalf("expected mfa_required+token, got res=%v err=%v", res, err)
	}
	// complete the gate with a live code
	code, _ := totp.GenerateCode(key.Secret(), time.Now())
	fin, err := s.CompleteMFALogin(ctx, res.MFAToken, code)
	if err != nil || fin.Token == "" {
		t.Fatalf("mfa complete: %v", err)
	}
	// gate token is single-use
	if _, err := s.CompleteMFALogin(ctx, res.MFAToken, code); err != ErrMFAGateInvalid {
		t.Fatalf("gate reuse should fail, got %v", err)
	}
	// wrong code inline → invalid_mfa_code
	if _, err = s.Login(ctx, email, "password1", "000000", "", "127.0.0.1", "ua"); err != ErrMFACode {
		t.Fatalf("expected invalid_mfa_code, got %v", err)
	}
	// correct code inline → token
	code, _ = totp.GenerateCode(key.Secret(), time.Now())
	if _, err = s.Login(ctx, email, "password1", code, "", "127.0.0.1", "ua"); err != nil {
		t.Fatalf("mfa login: %v", err)
	}
}

func TestPrivilegedUserNeedsMFASetup(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	u, email := verifiedUser(t, s, "adm")

	// grant org_admin system role → §02 privileged login requires MFA
	var roleID pgtype.UUID
	if err := s.pool.QueryRow(ctx, "SELECT id FROM user_roles WHERE slug='org_admin' AND tenant_id IS NULL").Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO user_role_assignments (user_id, role_id) VALUES ($1,$2)", toPg(u.ID), roleID); err != nil {
		t.Fatal(err)
	}
	if req, err := s.q.UserRequiresMFA(ctx, toPg(u.ID)); err != nil || !req {
		t.Fatalf("requires mfa: %v %v", req, err)
	}
	res, err := s.Login(ctx, email, "password1", "", "", "127.0.0.1", "ua")
	if err != ErrMFASetupRequired || res == nil || res.MFAToken == "" {
		t.Fatalf("expected mfa_setup_required, got %v", err)
	}
	// gate nonce resolves to the user for enrollment routes
	if uid, err := s.ResolveMFAGate(ctx, res.MFAToken); err != nil || uid != u.ID {
		t.Fatalf("gate resolve: %v %v", uid, err)
	}
}

func TestActionTokenSingleUse(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	tok, _ := SignActionToken("test-secret", "uid-x", "password_reset", time.Hour)
	if _, err := s.ConsumeActionToken(ctx, tok, "password_reset"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeActionToken(ctx, tok, "password_reset"); err == nil {
		t.Fatal("token reused — single-use not enforced")
	}
}

func TestUnverifiedLoginBlocked(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	email := "uv-" + uuid.NewString()[:8] + "@oga9.test"
	if _, _, err := s.Register(ctx, email, "password1", "UV"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, email, "password1", "", "", "127.0.0.1", "ua"); err != ErrEmailNotVerified {
		t.Fatalf("expected email_not_verified, got %v", err)
	}
}

func TestMFARecoveryAndDisable(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	u, email := verifiedUser(t, s, "rc")

	key, _ := totp.Generate(totp.GenerateOpts{Issuer: "T", AccountName: email})
	codes, err := s.SetupMFA(ctx, u.ID, key.Secret())
	if err != nil || len(codes) != 8 {
		t.Fatalf("setup: %v codes=%d", err, len(codes))
	}

	// recovery code works at login, once
	if _, err = s.Login(ctx, email, "password1", codes[0], "", "127.0.0.1", "ua"); err != nil {
		t.Fatalf("recovery login: %v", err)
	}
	if _, err = s.Login(ctx, email, "password1", codes[0], "", "127.0.0.1", "ua"); err != ErrMFACode {
		t.Fatalf("recovery reuse should fail, got %v", err)
	}

	// disable on a non-MFA account → mfa_not_enabled (not a 500 scan error)
	nu, _ := verifiedUser(t, s, "nomfa")
	if err := s.DisableMFA(ctx, nu.ID, "password1", "000000"); err != ErrMFANotEnabled {
		t.Fatalf("expected mfa_not_enabled, got %v", err)
	}

	// disable needs password + live code
	if err := s.DisableMFA(ctx, u.ID, "wrongpw1", "000000"); err != ErrBadCredentials {
		t.Fatalf("bad password: %v", err)
	}
	live, _ := totp.GenerateCode(key.Secret(), time.Now())
	if err := s.DisableMFA(ctx, u.ID, "password1", live); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// recovery codes cleared on disable
	if n, _ := db.New(s.pool).GetRecoveryCodes(ctx, toPg(u.ID)); string(n) != "[]" {
		t.Fatal("recovery codes not cleared on disable")
	}
	// password-only login works again
	if _, err = s.Login(ctx, email, "password1", "", "", "127.0.0.1", "ua"); err != nil {
		t.Fatalf("post-disable login: %v", err)
	}
}

func TestEmitAbortsTxOnFailure(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	// an invalid event payload (unmarshalable) must abort — state must not commit
	err := s.withTx(ctx, func(q *db.Queries) error {
		if e := q.SetUserVerified(ctx, toPg(uuid.New())); e != nil {
			return e
		}
		return emit(ctx, q, "UserRegistered", "user", uuid.New(), uuid.New(),
			map[string]any{"bad": func() {}})
	})
	if err == nil {
		t.Fatal("emit failure swallowed")
	}
}

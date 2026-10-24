package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"
	valkey "github.com/valkey-io/valkey-go"
)

// ---------- pure unit tests (no infra) ----------

func TestPasswordHashRoundtrip(t *testing.T) {
	h, err := HashPassword("s3cure-pw")
	if err != nil { t.Fatal(err) }
	if !CheckPassword(h, "s3cure-pw") { t.Fatal("roundtrip failed") }
	if CheckPassword(h, "wrong") { t.Fatal("wrong password accepted") }
}

func TestValidatePassword(t *testing.T) {
	for _, bad := range []string{"", "short1", "onlyletters", "12345678"} {
		if err := ValidatePassword(bad); err != ErrWeakPassword {
			t.Fatalf("%q should be weak", bad)
		}
	}
	if err := ValidatePassword("goodpass1"); err != nil { t.Fatal(err) }
}

func TestJWTRoundtrip(t *testing.T) {
	uid, jti := uuid.New(), uuid.New()
	tok, err := IssueToken("sec", uid, jti, time.Hour)
	if err != nil { t.Fatal(err) }
	c, err := ParseToken("sec", tok)
	if err != nil { t.Fatal(err) }
	if c.Subject != uid.String() || c.ID != jti.String() { t.Fatal("claims mismatch") }
	if _, err := ParseToken("other-secret", tok); err == nil { t.Fatal("wrong secret accepted") }
}

func TestActionTokenPurpose(t *testing.T) {
	tok, _ := SignActionToken("sec", "uid-1", "verify_email", time.Hour)
	if _, _, err := VerifyActionToken("sec", tok, "verify_email"); err != nil { t.Fatal(err) }
	if _, _, err := VerifyActionToken("sec", tok, "password_reset"); err == nil {
		t.Fatal("cross-purpose token accepted")
	}
}

// ---------- integration tests (need live stack) ----------

func testSvc(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" { t.Skip("OGA9_TEST_DATABASE_URL not set") }
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil { t.Fatal(err) }
	t.Cleanup(pool.Close)
	vk, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{envOr("OGA9_TEST_VALKEY_ADDR", "localhost:6379")}})
	if err != nil { t.Fatal(err) }
	return NewService(pool, vk, "test-secret", time.Hour)
}

func envOr(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }

func TestRegisterLoginLogoutFlow(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	email := "it-" + uuid.NewString()[:8] + "@oga9.test"

	u, _, err := s.Register(ctx, email, "password1", "IT User")
	if err != nil { t.Fatalf("register: %v", err) }
	if u.Email != email { t.Fatal("email mismatch") }

	// weak password rejected
	if _, _, err = s.Register(ctx, "w-"+uuid.NewString()[:8]+"@t.t", "weak", "x"); err != ErrWeakPassword {
		t.Fatal("weak password not rejected")
	}
	// duplicate email
	if _, _, err = s.Register(ctx, email, "password1", "x"); err != ErrEmailTaken {
		t.Fatalf("dup email: %v", err)
	}
	// bad login
	if _, _, err = s.Login(ctx, email, "wrongpw1", "", "", "127.0.0.1", "ua"); err != ErrBadCredentials {
		t.Fatalf("bad login: %v", err)
	}
	// good login
	token, sessID, err := s.Login(ctx, email, "password1", "", "dev", "127.0.0.1", "ua")
	if err != nil { t.Fatalf("login: %v", err) }
	claims, err := ParseToken("test-secret", token)
	if err != nil { t.Fatal(err) }
	// session resolves by jti
	if _, err = s.ResolveSession(ctx, uuid.MustParse(claims.ID)); err != nil { t.Fatalf("resolve: %v", err) }
	// logout revokes
	if err = s.RevokeSession(ctx, u.ID, sessID); err != nil { t.Fatal(err) }
	if _, err = s.ResolveSession(ctx, uuid.MustParse(claims.ID)); err == nil {
		t.Fatal("revoked session still resolves")
	}
}

func TestMFALoginEnforcement(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	email := "mfa-" + uuid.NewString()[:8] + "@oga9.test"
	u, _, err := s.Register(ctx, email, "password1", "MFA User")
	if err != nil { t.Fatal(err) }

	key, err := totp.Generate(totp.GenerateOpts{Issuer: "T", AccountName: email})
	if err != nil { t.Fatal(err) }
	if err := s.SetupMFA(ctx, u.ID, key.Secret()); err != nil { t.Fatal(err) }

	// no code → mfa_required
	if _, _, err = s.Login(ctx, email, "password1", "", "", "127.0.0.1", "ua"); err != ErrMFARequired {
		t.Fatalf("expected mfa_required, got %v", err)
	}
	// wrong code → invalid_mfa_code
	if _, _, err = s.Login(ctx, email, "password1", "000000", "", "127.0.0.1", "ua"); err != ErrMFACode {
		t.Fatalf("expected invalid_mfa_code, got %v", err)
	}
	// correct code → token
	code, err := totp.GenerateCode(key.Secret(), time.Now())
	if err != nil { t.Fatal(err) }
	if _, _, err = s.Login(ctx, email, "password1", code, "", "127.0.0.1", "ua"); err != nil {
		t.Fatalf("mfa login: %v", err)
	}
}

func TestActionTokenSingleUse(t *testing.T) {
	s := testSvc(t)
	ctx := context.Background()
	tok, _ := SignActionToken("test-secret", "uid-x", "password_reset", time.Hour)
	if _, err := s.ConsumeActionToken(ctx, tok, "password_reset"); err != nil { t.Fatal(err) }
	if _, err := s.ConsumeActionToken(ctx, tok, "password_reset"); err == nil {
		t.Fatal("token reused — single-use not enforced")
	}
}

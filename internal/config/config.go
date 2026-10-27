// Ref: oga9-engineering/sections/01-foundation-tech-stack.md
// All configurable values come from OGA9_* env vars — never hardcoded (Rule 3).
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env                  string
	Edition              string // cloud|self_hosted — §09 one codebase, two editions
	DatabaseURL          string
	ValkeyAddr           string
	JWTSecret            string
	DataKey              []byte // 32B AES key for secrets at rest (mfa_secret etc.)
	BaseDomain           string
	ControlHost          string
	APIPort              int
	UploadsDir           string
	UploadMaxMB          int64
	LiveKitURL           string
	LiveKitAPIKey        string
	LiveKitAPISecret     string
	VoiceJoinWindowSec   int
	VoiceRoomMaxAgeHours int
	// §22 ai — OpenAI-compatible provider endpoint (empty = unavailable).
	AIProviderURL   string
	AIProviderKey   string
	AIProviderModel string
	TrialDays       int // §15 default provision trial length (operator may override per request)
	SessionTTL      time.Duration
	OutboxInterval  time.Duration // events:dispatch_outbox cadence (§05)
	// §08 job queue cadences
	SchedulerTickInterval   time.Duration // scheduler:tick cron driver
	RetryDeadInterval       time.Duration // events:retry_dead DLQ re-drive
	BillingSweepInterval    time.Duration // billing:subscription_state_sweep cadence (§07)
	LicenseSignKey          []byte        // ed25519 seed for self-hosted license files (§07, Rule 29)
	LicenseFile             string        // §14 self-host: signed license file path (deployment-side)
	LicensePublicKey        string        // §14 self-host: hex ed25519 public key for offline verify
	LicenseOfflineGraceDays int           // license.offline_grace_days — warn-window past expiry (§09)
	WebhookSecret           []byte        // billing webhook signature secret (Rule 9)
	SMTPHost                string
	SMTPPort                int
	SMTPUser                string
	SMTPPass                string
	GlitchTipDSN            string
	MeilisearchURL          string
	MeilisearchKey          string
	S3Endpoint              string
	S3Bucket                string
	S3AccessKey             string
	S3SecretKey             string
	ImgproxyURL             string
	BaseURL                 string
	AppURL                  string // user-facing app origin for emailed links (app.oga9.com)
	MailFrom                string
	TrustedProxies          []*net.IPNet
}

// OAuthProviderEnv is a raw provider env bundle; cmd/api maps it onto
// auth.ProviderConfig so config stays free of domain-package imports.
type OAuthProviderEnv struct {
	ClientID, ClientSecret, AuthURL, TokenURL, UserinfoURL string
}

func Load() (*Config, error) {
	var errs []error
	cfg := &Config{
		Env:                     env("OGA9_ENV", "development"),
		Edition:                 env("OGA9_EDITION", "cloud"),
		DatabaseURL:             reqEnv(&errs, "OGA9_DATABASE_URL"),
		ValkeyAddr:              reqEnv(&errs, "OGA9_VALKEY_ADDR"),
		JWTSecret:               reqEnv(&errs, "OGA9_JWT_SECRET"),
		BaseDomain:              env("OGA9_BASE_DOMAIN", "oga9.com"),
		ControlHost:             env("OGA9_CONTROL_HOST", "control.oga9.com"),
		APIPort:                 envInt(&errs, "OGA9_API_PORT", 8080),
		SessionTTL:              envDur(&errs, "OGA9_SESSION_TTL", 24*time.Hour),
		OutboxInterval:          envDur(&errs, "OGA9_OUTBOX_DISPATCH_INTERVAL", 10*time.Second),
		UploadsDir:              env("OGA9_UPLOADS_DIR", "./data/uploads"),
		UploadMaxMB:             int64(envInt(&errs, "OGA9_UPLOAD_MAX_MB", 0)),
		LiveKitURL:              env("OGA9_LIVEKIT_URL", ""),
		LiveKitAPIKey:           env("OGA9_LIVEKIT_API_KEY", ""),
		LiveKitAPISecret:        env("OGA9_LIVEKIT_API_SECRET", ""),
		VoiceJoinWindowSec:      envInt(&errs, "OGA9_VOICE_JOIN_WINDOW_SEC", 300),
		VoiceRoomMaxAgeHours:    envInt(&errs, "OGA9_VOICE_ROOM_MAX_AGE_HOURS", 12),
		AIProviderURL:           env("OGA9_AI_PROVIDER_URL", ""),
		AIProviderKey:           env("OGA9_AI_API_KEY", ""),
		AIProviderModel:         env("OGA9_AI_MODEL", "llama3"),
		TrialDays:               envInt(&errs, "OGA9_TRIAL_DAYS", 14),
		LicenseFile:             env("OGA9_LICENSE_FILE", ""),
		LicensePublicKey:        env("OGA9_LICENSE_PUBLIC_KEY", ""),
		LicenseOfflineGraceDays: envInt(&errs, "OGA9_LICENSE_OFFLINE_GRACE_DAYS", 30),
		SchedulerTickInterval:   envDur(&errs, "OGA9_SCHEDULER_TICK_INTERVAL", 5*time.Minute),
		RetryDeadInterval:       envDur(&errs, "OGA9_EVENTS_RETRY_DEAD_INTERVAL", time.Minute),
		BillingSweepInterval:    envDur(&errs, "OGA9_BILLING_SWEEP_INTERVAL", time.Hour),
		SMTPHost:                env("OGA9_SMTP_HOST", "localhost"),
		SMTPPort:                envInt(&errs, "OGA9_SMTP_PORT", 1025),
		SMTPUser:                env("OGA9_SMTP_USERNAME", ""),
		SMTPPass:                env("OGA9_SMTP_PASSWORD", ""),
		GlitchTipDSN:            env("OGA9_GLITCHTIP_DSN", ""),
		MeilisearchURL:          env("OGA9_MEILISEARCH_URL", ""),
		MeilisearchKey:          env("OGA9_MEILISEARCH_KEY", ""),
		S3Endpoint:              env("OGA9_S3_ENDPOINT", ""),
		S3Bucket:                env("OGA9_S3_BUCKET", ""),
		S3AccessKey:             env("OGA9_S3_ACCESS_KEY", ""),
		S3SecretKey:             env("OGA9_S3_SECRET_KEY", ""),
		ImgproxyURL:             env("OGA9_IMGPROXY_URL", ""),
		MailFrom:                env("OGA9_MAIL_FROM", "noreply@oga9.com"),
		BaseURL:                 env("OGA9_BASE_URL", "http://localhost:8080"),
	}
	cfg.AppURL = env("OGA9_APP_URL", cfg.BaseURL)
	for _, c := range strings.Split(env("OGA9_TRUSTED_PROXIES", ""), ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(c); err != nil {
			errs = append(errs, fmt.Errorf("OGA9_TRUSTED_PROXIES: bad CIDR %q", c))
		} else {
			cfg.TrustedProxies = append(cfg.TrustedProxies, n)
		}
	}
	cfg.DataKey = dataKey(env("OGA9_DATA_ENCRYPTION_KEY", ""), cfg.JWTSecret)
	cfg.LicenseSignKey = licenseSeed(env("OGA9_LICENSE_SIGNING_KEY", ""), cfg.JWTSecret)
	if cfg.Edition == "self_hosted" {
		// defense in depth (Rule 29): even if an env var were set, a
		// self-hosted deployment never signs — IssueLicense fails
		// license_signing_unconfigured.
		cfg.LicenseSignKey = nil
	}
	cfg.WebhookSecret = []byte(env("OGA9_BILLING_WEBHOOK_SECRET", "")) // empty → endpoint rejects providers
	if cfg.Edition != "cloud" && cfg.Edition != "self_hosted" {
		errs = append(errs, fmt.Errorf("OGA9_EDITION must be cloud or self_hosted, got %q", cfg.Edition))
	}
	if cfg.JWTSecret == "change-me" && cfg.Env == "production" {
		errs = append(errs, errors.New("OGA9_JWT_SECRET must be set in production"))
	}
	// The signing key is control-plane material (§09): only the cloud
	// edition signs licenses. A self-hosted deployment must NEVER hold it —
	// it verifies the signed file offline with the public key (Rule 29).
	if cfg.Env == "production" && cfg.Edition == "cloud" && env("OGA9_LICENSE_SIGNING_KEY", "") == "" {
		errs = append(errs, errors.New("OGA9_LICENSE_SIGNING_KEY must be set in production (cloud edition)"))
	}
	if cfg.JWTSecret == "" {
		errs = append(errs, errors.New("OGA9_JWT_SECRET is empty"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// dataKey resolves the 32-byte at-rest encryption key: explicit
// OGA9_DATA_ENCRYPTION_KEY (hex/base64/raw) wins; otherwise derive a stable
// key from the JWT secret so dev installs need no extra env.
func dataKey(raw, jwtSecret string) []byte {
	sum := sha256.Sum256([]byte("oga9-data-key|" + jwtSecret))
	if raw != "" {
		if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
			sum = sha256.Sum256(b)
		} else if b, err := hex.DecodeString(raw); err == nil {
			sum = sha256.Sum256(b)
		} else {
			sum = sha256.Sum256([]byte(raw))
		}
	}
	k := make([]byte, 32)
	copy(k, sum[:])
	return k
}

// licenseSeed resolves the 32-byte ed25519 seed for license signing:
// explicit OGA9_LICENSE_SIGNING_KEY (hex/base64/raw) wins; otherwise a stable
// derivation from the JWT secret keeps dev installs zero-config. Only the
// public half ships to self-hosted deployments (Rule 29).
func licenseSeed(raw, jwtSecret string) []byte {
	sum := sha256.Sum256([]byte("oga9-license-seed|" + jwtSecret))
	if raw != "" {
		if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
			sum = sha256.Sum256(b)
		} else if b, err := hex.DecodeString(raw); err == nil {
			sum = sha256.Sum256(b)
		} else {
			sum = sha256.Sum256([]byte(raw))
		}
	}
	return sum[:]
}

func (c *Config) IsProduction() bool { return c.Env == "production" }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func reqEnv(errs *[]error, key string) string {
	v := os.Getenv(key)
	if v == "" {
		*errs = append(*errs, fmt.Errorf("required env var %s is not set", key))
	}
	return v
}

func envDur(errs *[]error, key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return d
}

func envInt(errs *[]error, key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return fallback
	}
	return n
}

// OAuthProviders builds the configured provider map — a provider activates
// only when its client id+secret env vars are set (§02).
func (c *Config) OAuthProviders() map[string]OAuthProviderEnv {
	defs := map[string]map[string]string{
		"google": {
			"auth":     "https://accounts.google.com/o/oauth2/v2/auth",
			"token":    "https://oauth2.googleapis.com/token",
			"userinfo": "https://openidconnect.googleapis.com/v1/userinfo",
		},
		"japaconnect": {}, // URLs required via env (ADR-021 endpoint TBD)
	}
	out := map[string]OAuthProviderEnv{}
	for name, urls := range defs {
		id := os.Getenv("OGA9_OAUTH_" + strings.ToUpper(name) + "_CLIENT_ID")
		secret := os.Getenv("OGA9_OAUTH_" + strings.ToUpper(name) + "_CLIENT_SECRET")
		if id == "" || secret == "" {
			continue
		}
		out[name] = OAuthProviderEnv{
			ClientID:     id,
			ClientSecret: secret,
			AuthURL:      env("OGA9_OAUTH_"+strings.ToUpper(name)+"_AUTH_URL", urls["auth"]),
			TokenURL:     env("OGA9_OAUTH_"+strings.ToUpper(name)+"_TOKEN_URL", urls["token"]),
			UserinfoURL:  env("OGA9_OAUTH_"+strings.ToUpper(name)+"_USERINFO_URL", urls["userinfo"]),
		}
	}
	return out
}

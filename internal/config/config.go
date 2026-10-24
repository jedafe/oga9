// Ref: oga9-engineering/sections/01-foundation-tech-stack.md
// All configurable values come from OGA9_* env vars — never hardcoded (Rule 3).
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/jedafe/oga9/internal/auth"
	"strconv"
	"time"
)

type Config struct {
	Env            string
	DatabaseURL    string
	ValkeyAddr     string
	JWTSecret      string
	BaseDomain     string
	ControlHost    string
	APIPort        int
	SessionTTL     time.Duration
	SMTPHost       string
	SMTPPort       int
	GlitchTipDSN   string
	MeilisearchURL string
	MeilisearchKey string
	S3Endpoint     string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	ImgproxyURL    string
	BaseURL        string
}

func Load() (*Config, error) {
	cfg := &Config{
		Env:            env("OGA9_ENV", "development"),
		DatabaseURL:    mustEnv("OGA9_DATABASE_URL"),
		ValkeyAddr:     mustEnv("OGA9_VALKEY_ADDR"),
		JWTSecret:      mustEnv("OGA9_JWT_SECRET"),
		BaseDomain:     env("OGA9_BASE_DOMAIN", "oga9.com"),
		ControlHost:    env("OGA9_CONTROL_HOST", "control.oga9.com"),
		APIPort:        envInt("OGA9_API_PORT", 8080),
		SessionTTL:     envDur("OGA9_SESSION_TTL", 24*time.Hour),
		SMTPHost:       env("OGA9_SMTP_HOST", "localhost"),
		SMTPPort:       envInt("OGA9_SMTP_PORT", 1025),
		GlitchTipDSN:   env("OGA9_GLITCHTIP_DSN", ""),
		MeilisearchURL: env("OGA9_MEILISEARCH_URL", ""),
		MeilisearchKey: env("OGA9_MEILISEARCH_KEY", ""),
		S3Endpoint:     env("OGA9_S3_ENDPOINT", ""),
		S3Bucket:       env("OGA9_S3_BUCKET", ""),
		S3AccessKey:    env("OGA9_S3_ACCESS_KEY", ""),
		S3SecretKey:    env("OGA9_S3_SECRET_KEY", ""),
		ImgproxyURL:    env("OGA9_IMGPROXY_URL", ""),
		BaseURL:        env("OGA9_BASE_URL", "http://localhost:8080"),
	}
	if cfg.JWTSecret == "change-me" && cfg.Env == "production" {
		return nil, fmt.Errorf("OGA9_JWT_SECRET must be set in production")
	}
	return cfg, nil
}

func (c *Config) IsProduction() bool { return c.Env == "production" }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("required env var %s is not set", key))
	}
	return v
}

func envDur(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// OAuthProviders builds the configured provider map — a provider activates
// only when its client id+secret env vars are set (§02).
func (c *Config) OAuthProviders() map[string]auth.ProviderConfig {
	defs := map[string]map[string]string{
		"google": {
			"auth":     "https://accounts.google.com/o/oauth2/v2/auth",
			"token":    "https://oauth2.googleapis.com/token",
			"userinfo": "https://openidconnect.googleapis.com/v1/userinfo",
		},
		"japaconnect": {}, // URLs required via env (ADR-021 endpoint TBD)
	}
	out := map[string]auth.ProviderConfig{}
	for name, urls := range defs {
		id := os.Getenv("OGA9_OAUTH_" + strings.ToUpper(name) + "_CLIENT_ID")
		secret := os.Getenv("OGA9_OAUTH_" + strings.ToUpper(name) + "_CLIENT_SECRET")
		if id == "" || secret == "" {
			continue
		}
		out[name] = auth.ProviderConfig{
			ClientID:     id,
			ClientSecret: secret,
			AuthURL:      env("OGA9_OAUTH_"+strings.ToUpper(name)+"_AUTH_URL", urls["auth"]),
			TokenURL:     env("OGA9_OAUTH_"+strings.ToUpper(name)+"_TOKEN_URL", urls["token"]),
			UserinfoURL:  env("OGA9_OAUTH_"+strings.ToUpper(name)+"_USERINFO_URL", urls["userinfo"]),
		}
	}
	return out
}

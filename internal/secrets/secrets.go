// Ref: oga9-engineering/sections/23-integrations.md — credential_ref is a
// POINTER into a secrets backend, never a stored secret. Supported
// schemes keep secrets out of PostgreSQL at every layer.
package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Resolve dereferences a credential_ref:
//
//	env:VAR_NAME      → os.Getenv(VAR_NAME)        — runtime env (dev+prod)
//	file:/abs/path    → file contents              — docker/k8s secret mounts
//
// A bare value (no scheme) is REJECTED — storing a raw secret in
// credential_ref would violate "secrets never in DB". Callers must move
// it into an env var or mounted file.
func Resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("empty credential_ref")
	}
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("credential_ref %q has no scheme — use env: or file: (raw secrets are never stored)", truncate(ref))
	}
	switch scheme {
	case "env":
		v := os.Getenv(rest)
		if v == "" {
			return "", fmt.Errorf("env secret %q unset", rest)
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(rest)
		if err != nil {
			return "", fmt.Errorf("secret file %q: %w", rest, err)
		}
		return strings.TrimSpace(string(b)), nil
	default:
		return "", fmt.Errorf("unsupported credential_ref scheme %q — use env: or file:", scheme)
	}
}

func truncate(s string) string {
	if len(s) > 8 {
		return s[:4] + "…"
	}
	return s
}

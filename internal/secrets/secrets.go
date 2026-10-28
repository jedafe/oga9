// Ref: oga9-engineering/sections/23-integrations.md — credential_ref is a
// POINTER into a secrets backend, never a stored secret. Supported
// schemes keep secrets out of PostgreSQL at every layer.
package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// fileRoot constrains file: refs to one configured directory
// (OGA9_SECRETS_DIR — docker/k8s secret mounts). Scoped refs are written
// by org-side actors; an unbounded path would let them read any file the
// API process can open (/proc/self/environ alone leaks every env secret).
var (
	fileRootMu sync.RWMutex
	fileRoot   string
)

// SetFileRoot sets the directory file: refs must resolve inside.
// Empty → file: refs are rejected entirely (fail closed).
func SetFileRoot(dir string) {
	fileRootMu.Lock()
	defer fileRootMu.Unlock()
	if dir == "" {
		fileRoot = ""
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fileRoot = ""
		return
	}
	fileRoot = abs
}

func fileRootDir() string {
	fileRootMu.RLock()
	defer fileRootMu.RUnlock()
	return fileRoot
}

// Resolve dereferences a credential_ref:
//
//	env:VAR_NAME      → os.Getenv(VAR_NAME)        — runtime env (dev+prod)
//	file:/abs/path    → file contents              — under OGA9_SECRETS_DIR only
//
// A bare value (no scheme) is REJECTED — storing a raw secret in
// credential_ref would violate "secrets never in DB". Callers must move
// it into an env var or mounted file. OGA9_* env names are the platform
// config namespace (Rule 3) and are never resolvable through a scoped
// ref — connector secrets live in their own variables.
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
		if rest == "" || strings.HasPrefix(rest, "OGA9_") {
			return "", fmt.Errorf("env secret %q not resolvable through credential_ref — OGA9_* is platform config", truncate(rest))
		}
		v := os.Getenv(rest)
		if v == "" {
			return "", fmt.Errorf("env secret %q unset", rest)
		}
		return v, nil
	case "file":
		root := fileRootDir()
		if root == "" {
			return "", fmt.Errorf("file credential_refs require OGA9_SECRETS_DIR to be configured")
		}
		p, err := filepath.Abs(rest)
		if err != nil {
			return "", fmt.Errorf("secret file %q: %w", rest, err)
		}
		if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
			return "", fmt.Errorf("secret file %q escapes the configured secrets directory", truncate(rest))
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("secret file %q: %w", truncate(rest), err)
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

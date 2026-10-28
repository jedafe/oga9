package secrets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	ctx := context.Background()
	os.Setenv("TEST_SECRET_X", "s3cret")
	defer os.Unsetenv("TEST_SECRET_X")
	if v, err := Resolve(ctx, "env:TEST_SECRET_X"); err != nil || v != "s3cret" {
		t.Fatalf("env: %v %q", err, v)
	}
	// OGA9_* is the platform namespace — never resolvable via a scoped ref.
	os.Setenv("OGA9_TEST_PLATFORM", "platform")
	defer os.Unsetenv("OGA9_TEST_PLATFORM")
	if _, err := Resolve(ctx, "env:OGA9_TEST_PLATFORM"); err == nil {
		t.Fatal("OGA9_* env refs must be rejected")
	}
	// file: refs are confined to the configured root.
	dir := t.TempDir()
	SetFileRoot(dir)
	defer SetFileRoot("")
	f := filepath.Join(dir, "sec")
	os.WriteFile(f, []byte("filesecret\n"), 0600)
	if v, err := Resolve(ctx, "file:"+f); err != nil || v != "filesecret" {
		t.Fatalf("file: %v %q", err, v)
	}
	outside := filepath.Join(t.TempDir(), "out")
	os.WriteFile(outside, []byte("nope"), 0600)
	if _, err := Resolve(ctx, "file:"+outside); err == nil {
		t.Fatal("file: refs outside the secrets dir must be rejected")
	}
	if _, err := Resolve(ctx, "file:"+filepath.Join(dir, "..", "out")); err == nil {
		t.Fatal("file: refs escaping via .. must be rejected")
	}
	if _, err := Resolve(ctx, "plaintextsecret"); err == nil {
		t.Fatal("raw secret must be rejected")
	}
	if _, err := Resolve(ctx, "env:TEST_SECRET_MISSING"); err == nil {
		t.Fatal("unset env must fail")
	}
}

func TestFileRefFailsClosedWithoutRoot(t *testing.T) {
	SetFileRoot("")
	if _, err := Resolve(context.Background(), "file:/tmp/anything"); err == nil {
		t.Fatal("file: refs must fail closed when no secrets dir is configured")
	}
}

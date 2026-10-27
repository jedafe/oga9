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
	f := filepath.Join(t.TempDir(), "sec")
	os.WriteFile(f, []byte("filesecret\n"), 0600)
	if v, err := Resolve(ctx, "file:"+f); err != nil || v != "filesecret" {
		t.Fatalf("file: %v %q", err, v)
	}
	if _, err := Resolve(ctx, "plaintextsecret"); err == nil {
		t.Fatal("raw secret must be rejected")
	}
	if _, err := Resolve(ctx, "env:TEST_SECRET_MISSING"); err == nil {
		t.Fatal("unset env must fail")
	}
}

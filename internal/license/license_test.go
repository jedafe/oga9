// Ref: oga9-engineering/sections/09-deployment-editions.md — §14.03 round
// trip: a file signed the way billing.Service.IssueLicense signs it must
// verify offline; tampering/expiry/grace degrade per the matrix.
package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func signedFile(t *testing.T, priv ed25519.PrivateKey, f *File) string {
	t.Helper()
	f.Signature = hex.EncodeToString(ed25519.Sign(priv, payload(f)))
	b, _ := json.Marshal(f)
	p := filepath.Join(t.TempDir(), "license.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubHex := hex.EncodeToString(pub)

	cases := []struct {
		name  string
		f     *File
		grace int
		want  Status
	}{
		{"valid", &File{DeploymentID: "dep-1", Edition: "business",
			Entitlements: json.RawMessage(`{"capabilities":["app.events_meetings"],"limits":{"limit.storage.mb":10240}}`),
			ExpiresAt:    time.Now().Add(72 * time.Hour)}, 30, StatusValid},
		{"grace", &File{DeploymentID: "dep-1", Edition: "business",
			ExpiresAt: time.Now().Add(-24 * time.Hour)}, 30, StatusGrace},
		{"expired", &File{DeploymentID: "dep-1", Edition: "business",
			ExpiresAt: time.Now().Add(-40 * 24 * time.Hour)}, 30, StatusExpired},
	}
	for _, c := range cases {
		v, err := New(pubHex, signedFile(t, priv, c.f), c.grace)
		if err != nil {
			t.Fatal(err)
		}
		got, f, err := v.Check()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %s want %s", c.name, got, c.want)
		}
		if f.DeploymentID != "dep-1" {
			t.Fatalf("%s: wrong deployment", c.name)
		}
	}
}

func TestCheckTamperedAndMissing(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubHex := hex.EncodeToString(pub)
	f := &File{DeploymentID: "dep-1", Entitlements: json.RawMessage(`{"a":1}`),
		ExpiresAt: time.Now().Add(time.Hour)}
	path := signedFile(t, priv, f)
	// tamper: entitlement bumped after signing
	f.Entitlements = json.RawMessage(`{"a":999}`)
	b, _ := json.Marshal(f)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	v, _ := New(pubHex, path, 30)
	if st, _, err := v.Check(); st != StatusInvalid || err == nil {
		t.Fatalf("tampered file: st=%s err=%v", st, err)
	}
	// no file / no key → community, never an error (Rule 29)
	for _, p := range []string{filepath.Join(t.TempDir(), "nope.json"), ""} {
		v, _ = New(pubHex, p, 30)
		if st, _, err := v.Check(); st != StatusCommunity || err != nil {
			t.Fatalf("missing file %q: st=%s err=%v", p, st, err)
		}
	}
	v, _ = New("", "/x", 30)
	if st, _, _ := v.Check(); st != StatusCommunity {
		t.Fatalf("no pubkey: %s", st)
	}
}

func TestEntitlement(t *testing.T) {
	f := &File{Entitlements: json.RawMessage(`{"capabilities":["app.events_meetings"],"limits":{"limit.storage.mb":10240}}`)}
	if v, ok := f.Entitlement("limit.storage.mb"); !ok || v != 10240 {
		t.Fatalf("limit: %v %v", v, ok)
	}
	if v, ok := f.Entitlement("app.events_meetings"); !ok || v != 1 {
		t.Fatalf("capability: %v %v", v, ok)
	}
	if _, ok := f.Entitlement("missing"); ok {
		t.Fatal("missing key should report !ok")
	}
}

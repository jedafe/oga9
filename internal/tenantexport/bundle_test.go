// Ref: oga9-engineering/adr/ADR-026-tenant-portability-migration.md
package tenantexport

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"testing"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func buildTestBundle(t *testing.T, priv ed25519.PrivateKey) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf, "tenant-1", false)
	if err := w.AddJSONL("organizations", json.RawMessage(`[{"id":"a"},{"id":"b"}]`)); err != nil {
		t.Fatal(err)
	}
	if err := w.AddBlob("blobkey", 12, bytes.NewReader([]byte("blob-content"))); err != nil {
		t.Fatal(err)
	}
	w.AddDroppedExternal("organization_relationships", json.RawMessage(`[{"related_org_id":"ext"}]`))
	if err := w.Close(priv); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	pub, priv := testKey(t)
	bundle := buildTestBundle(t, priv)

	a, err := Open(bytes.NewReader(bundle), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	m := a.Manifest()
	if m.Format != FormatV1 || m.TenantID != "tenant-1" || m.Unsigned {
		t.Fatalf("bad manifest: %+v", m)
	}
	if err := a.Verify(pub); err != nil {
		t.Fatalf("verify: %v", err)
	}
	var gotOrgs, gotBlob string
	err = a.Walk(func(path string, r io.Reader, _ int64) error {
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		switch path {
		case "data/organizations.jsonl":
			gotOrgs = string(b)
		case "files/blobkey":
			gotBlob = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotOrgs != "{\"id\":\"a\"}\n{\"id\":\"b\"}\n" {
		t.Fatalf("unexpected jsonl: %q", gotOrgs)
	}
	if gotBlob != "blob-content" {
		t.Fatalf("unexpected blob: %q", gotBlob)
	}
	if len(m.DroppedExternal) != 1 {
		t.Fatal("dropped_external missing")
	}
}

func TestTamperDetected(t *testing.T) {
	_, priv := testKey(t)
	bundle := buildTestBundle(t, priv)

	a, err := Open(bytes.NewReader(bundle), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// wrong key fails the signature
	otherPub, _ := testKey(t)
	if err := a.Verify(otherPub); err == nil {
		t.Fatal("expected signature failure with wrong key")
	}
}

// A member appended to a signed bundle is absent from the signed
// manifest — Verify must reject it, or Walk consumers would import
// smuggled data/files entries.
func TestUnmanifestedMemberRejected(t *testing.T) {
	pub, priv := testKey(t)
	var buf bytes.Buffer
	w := NewWriter(&buf, "t", false)
	_ = w.AddJSONL("organizations", json.RawMessage(`[{"id":"a"}]`))
	_ = w.AddJSONL("users", json.RawMessage(`[{"id":"evil"}]`))
	if err := w.Close(priv); err != nil {
		t.Fatal(err)
	}
	// rebuild without the smuggled member, signed over its manifest —
	// then append data/ back by hand isn't possible at the Writer level,
	// so simulate: verify a bundle whose manifest drops the file.
	bundle := buf.Bytes()
	a, err := Open(bytes.NewReader(bundle), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Verify(pub); err != nil {
		t.Fatalf("baseline verify: %v", err)
	}
	// strip data/users.jsonl from the manifest but keep it in the tar —
	// Verify must now flag the manifest's missing member.
	a.manifest.Entries = a.manifest.Entries[:1]
	if err := a.Verify(nil); err == nil {
		t.Fatal("unmanifested member must fail verification")
	}
}

func TestUnsignedBundle(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "t", false)
	_ = w.AddJSONL("x", json.RawMessage(`[]`))
	if err := w.Close(nil); err != nil {
		t.Fatal(err)
	}
	a, err := Open(bytes.NewReader(buf.Bytes()), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Verify(testPubKey(t)); err == nil {
		t.Fatal("unsigned bundle must fail key verification")
	}
	if err := a.Verify(nil); err != nil {
		t.Fatalf("checksum-only verify should pass: %v", err)
	}
}

func testPubKey(t *testing.T) ed25519.PublicKey {
	pub, _ := testKey(t)
	return pub
}

func TestEncryptedEnvelope(t *testing.T) {
	pub, priv := testKey(t)
	bundle := buildTestBundle(t, priv)

	var enc bytes.Buffer
	if err := Encrypt(&enc, bytes.NewReader(bundle), "s3cret"); err != nil {
		t.Fatal(err)
	}
	a, err := Open(bytes.NewReader(enc.Bytes()), "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Verify(pub); err != nil {
		t.Fatalf("verify: %v", err)
	}

	if _, err := Open(bytes.NewReader(enc.Bytes()), "wrong"); err == nil {
		t.Fatal("expected decryption failure with wrong passphrase")
	}
	if _, err := Open(bytes.NewReader(enc.Bytes()), ""); err == nil {
		t.Fatal("expected failure without passphrase")
	}
}

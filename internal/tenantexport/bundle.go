// Ref: oga9-engineering/adr/ADR-026-tenant-portability-migration.md
// tenant_export_v1 bundle: a gzip'd tar carrying manifest.json +
// signature + data/<table>.jsonl + files/<blob-key>. The platform signs
// manifest.json with the license ed25519 seed; every other member's
// sha256 rides inside the manifest, so one signature covers the whole
// archive. include_credentials bundles encrypt the entire tar with a
// tenant-supplied passphrase (argon2id → AES-256-GCM) under the
// tenant_export_v1_enc envelope.
package tenantexport

import (
	"archive/tar"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	FormatV1      = "tenant_export_v1"
	FormatV1Enc   = "tenant_export_v1_enc"
	BundleVersion = 1
)

// ManifestEntry is one archive member's integrity record — path, sha256,
// size, and a row count for data files.
type ManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Rows   int    `json:"rows,omitempty"`
}

// DroppedExternal records rows intentionally left out of the data set —
// cross-tenant organization_relationships edges whose related_org lives
// under a different tenant can't satisfy the FK on import.
type DroppedExternal struct {
	Table string          `json:"table"`
	Rows  json.RawMessage `json:"rows"`
}

// Manifest anchors the bundle; it is the signed material.
type Manifest struct {
	Format             string            `json:"format"`
	BundleVersion      int               `json:"bundle_version"`
	TenantID           string            `json:"tenant_id"`
	ExportedAt         time.Time         `json:"exported_at"`
	IncludeCredentials bool              `json:"include_credentials"`
	Unsigned           bool              `json:"unsigned,omitempty"`
	Entries            []ManifestEntry   `json:"entries"`
	DroppedExternal    []DroppedExternal `json:"dropped_external,omitempty"`
}

var ident = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ---------- writer ----------

// Writer streams archive members into a gzip'd tar and seals it with the
// manifest + signature on Close.
type Writer struct {
	tw       *tar.Writer
	gz       *gzip.Writer
	manifest Manifest
}

func NewWriter(dst io.Writer, tenantID string, includeCredentials bool) *Writer {
	gz := gzip.NewWriter(dst)
	return &Writer{
		tw: tar.NewWriter(gz),
		gz: gz,
		manifest: Manifest{
			Format:             FormatV1,
			BundleVersion:      BundleVersion,
			TenantID:           tenantID,
			ExportedAt:         time.Now().UTC(),
			IncludeCredentials: includeCredentials,
		},
	}
}

// AddJSONL writes a JSON array of row objects as data/<table>.jsonl —
// one document per line so importers can stream rows.
func (w *Writer) AddJSONL(table string, rows json.RawMessage) error {
	if !ident.MatchString(table) {
		return fmt.Errorf("bundle: bad table name %q", table)
	}
	var objs []json.RawMessage
	if err := json.Unmarshal(rows, &objs); err != nil {
		return fmt.Errorf("bundle: %s rows: %w", table, err)
	}
	var buf []byte
	for _, o := range objs {
		buf = append(buf, o...)
		buf = append(buf, '\n')
	}
	return w.add("data/"+table+".jsonl", buf, len(objs))
}

// AddBlob writes a blob-storage object as files/<key>.
func (w *Writer) AddBlob(key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return w.add("files/"+key, data, 0)
}

// AddDroppedExternal records rows excluded for cross-tenant integrity.
func (w *Writer) AddDroppedExternal(table string, rows json.RawMessage) {
	w.manifest.DroppedExternal = append(w.manifest.DroppedExternal,
		DroppedExternal{Table: table, Rows: rows})
}

func (w *Writer) add(path string, data []byte, rows int) error {
	sum := sha256.Sum256(data)
	if err := w.tw.WriteHeader(&tar.Header{
		Name: path, Mode: 0o644, Size: int64(len(data)),
	}); err != nil {
		return err
	}
	if _, err := w.tw.Write(data); err != nil {
		return err
	}
	w.manifest.Entries = append(w.manifest.Entries, ManifestEntry{
		Path: path, SHA256: hex.EncodeToString(sum[:]),
		Bytes: int64(len(data)), Rows: rows,
	})
	return nil
}

// Close writes manifest.json + signature and seals the archive. A nil
// private key produces an unsigned bundle (self-hosted installs hold no
// signing seed — the manifest marks it).
func (w *Writer) Close(priv ed25519.PrivateKey) error {
	w.manifest.Unsigned = len(priv) == 0
	mb, err := json.Marshal(w.manifest)
	if err != nil {
		return err
	}
	write := func(path string, data []byte) error {
		if err := w.tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err := w.tw.Write(data)
		return err
	}
	if err := write("manifest.json", mb); err != nil {
		return err
	}
	sig := ""
	if !w.manifest.Unsigned {
		sig = hex.EncodeToString(ed25519.Sign(priv, mb))
	}
	if err := write("signature", []byte(sig)); err != nil {
		return err
	}
	if err := w.tw.Close(); err != nil {
		return err
	}
	return w.gz.Close()
}

// ---------- reader ----------

// Archive is a parsed bundle spooled to a temp file — the manifest and
// signature are extracted eagerly; member reads walk the archive.
type Archive struct {
	tmp      string // temp file path, cleaned on Close
	manifest Manifest
	sig      string
}

// Open reads a bundle (raw tar.gz or a tenant_export_v1_enc envelope).
// passphrase is required when the envelope is encrypted.
func Open(r io.Reader, passphrase string) (*Archive, error) {
	head := make([]byte, 4096)
	n, _ := io.ReadFull(r, head)
	head = head[:n]
	body := io.MultiReader(io.NopCloser(bytesReader(head)), r)

	// tenant_export_v1_enc is a JSON envelope — sniff '{' vs gzip magic.
	if n > 0 && head[0] == '{' {
		raw, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		plain, err := decryptEnvelope(raw, passphrase)
		if err != nil {
			return nil, err
		}
		body = bytesReader(plain)
	}

	tmp, err := spoolTemp(body)
	if err != nil {
		return nil, err
	}
	a := &Archive{tmp: tmp}
	if err := a.loadManifest(); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// Manifest returns the parsed manifest — call Verify first to trust it.
func (a *Archive) Manifest() Manifest { return a.manifest }

// Verify checks the manifest signature against pub (when given), then
// every member's sha256 against the manifest entries. A nil pub skips
// the signature check — unsigned bundles pass, signed ones are only
// checksum-verified (callers decide if that's acceptable).
func (a *Archive) Verify(pub ed25519.PublicKey) error {
	if len(pub) > 0 {
		mb, err := a.readMember("manifest.json")
		if err != nil {
			return err
		}
		if a.manifest.Unsigned {
			return errors.New("bundle is unsigned")
		}
		sig, err := hex.DecodeString(a.sig)
		if err != nil || !ed25519.Verify(pub, mb, sig) {
			return errors.New("bundle signature invalid")
		}
	}
	want := map[string]ManifestEntry{}
	for _, e := range a.manifest.Entries {
		want[e.Path] = e
	}
	seen := map[string]bool{}
	err := a.walk(func(path string, r io.Reader, _ int64) error {
		if path == "manifest.json" || path == "signature" {
			return nil
		}
		e, ok := want[path]
		if !ok {
			// the signature only covers manifest.json — a member absent
			// from it could be appended to a signed bundle undetected,
			// then smuggled into Walk consumers (import).
			return fmt.Errorf("bundle: unmanifested member %s", path)
		}
		seen[path] = true
		h := sha256.New()
		if _, err := io.Copy(h, r); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return fmt.Errorf("bundle: checksum mismatch on %s", path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for p := range want {
		if !seen[p] {
			return fmt.Errorf("bundle: missing member %s", p)
		}
	}
	return nil
}

// Walk streams each member after verification — single pass, suitable
// for importing without holding the payload in memory.
func (a *Archive) Walk(fn func(path string, r io.Reader, size int64) error) error {
	return a.walk(fn)
}

func (a *Archive) Close() { removeTemp(a.tmp) }

func (a *Archive) loadManifest() error {
	return a.walk(func(path string, r io.Reader, _ int64) error {
		switch path {
		case "manifest.json":
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(b, &a.manifest); err != nil {
				return fmt.Errorf("bundle: bad manifest: %w", err)
			}
			if a.manifest.Format != FormatV1 {
				return fmt.Errorf("bundle: unsupported format %q", a.manifest.Format)
			}
		case "signature":
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			a.sig = string(b)
		}
		return nil
	})
}

func (a *Archive) readMember(path string) ([]byte, error) {
	var out []byte
	err := a.walk(func(p string, r io.Reader, _ int64) error {
		if p != path {
			return nil
		}
		b, err := io.ReadAll(r)
		out = b
		return err
	})
	if out == nil {
		return nil, fmt.Errorf("bundle: missing %s", path)
	}
	return out, err
}

func (a *Archive) walk(fn func(string, io.Reader, int64) error) error {
	f, err := openTemp(a.tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("bundle: not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := fn(h.Name, tr, h.Size); err != nil {
			return err
		}
	}
}

// ---------- passphrase envelope ----------

type envelope struct {
	Format  string `json:"format"`
	KDF     string `json:"kdf"`
	Salt    string `json:"salt"`
	Nonce   string `json:"nonce"`
	Payload string `json:"payload"`
}

// Encrypt wraps a finished tar.gz bundle in the tenant_export_v1_enc
// envelope — argon2id(passphrase) → AES-256-GCM.
func Encrypt(plain []byte, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("bundle: passphrase required for credentials export")
	}
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(passphrase), salt, 1, 64*1024, 4, 32)
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	env := envelope{
		Format:  FormatV1Enc,
		KDF:     "argon2id",
		Salt:    base64.StdEncoding.EncodeToString(salt),
		Nonce:   base64.StdEncoding.EncodeToString(nonce),
		Payload: base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plain, nil)),
	}
	return json.Marshal(env)
}

func decryptEnvelope(raw []byte, passphrase string) ([]byte, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bundle: bad envelope: %w", err)
	}
	if env.Format != FormatV1Enc {
		return nil, fmt.Errorf("bundle: unsupported envelope %q", env.Format)
	}
	salt, err := base64.StdEncoding.DecodeString(env.Salt)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, err
	}
	if passphrase == "" {
		return nil, errors.New("bundle is encrypted — passphrase required")
	}
	key := argon2.IDKey([]byte(passphrase), salt, 1, 64*1024, 4, 32)
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ct, nil)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

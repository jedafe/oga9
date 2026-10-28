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
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// AddBlob writes a blob-storage object as files/<key>, streaming the
// content through the sha256 — large blobs never buffer in memory.
func (w *Writer) AddBlob(key string, size int64, r io.Reader) error {
	path := "files/" + key
	h := sha256.New()
	if err := w.tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: size}); err != nil {
		return err
	}
	if _, err := io.Copy(w.tw, io.TeeReader(r, h)); err != nil {
		return err
	}
	w.manifest.Entries = append(w.manifest.Entries, ManifestEntry{
		Path: path, SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: size,
	})
	return nil
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
	head := make([]byte, len(encMagic))
	n, _ := io.ReadFull(r, head)
	head = head[:n]
	body := io.MultiReader(bytesReader(head), r)

	var (
		tmp string
		err error
	)
	if n == len(encMagic) && string(head) == encMagic {
		tmp, err = decryptToTemp(body, passphrase)
	} else {
		tmp, err = spoolTemp(body)
	}
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

// tenant_export_v1_enc is a STREAMING binary envelope — the whole tar is
// never held in memory on either side:
//
//	[8B  magic "OGA9ENC1"][16B argon2id salt][12B base nonce]
//	repeated: [4B BE ct_len][ct] — ct = AES-256-GCM seal of a ≤4MiB
//	plaintext chunk; nonce = base nonce XOR chunk counter (BE last 4B)
//
// A single key derives per-chunk nonces from a counter, so nonce reuse
// is impossible while chunking stays sequential.
const (
	encMagic     = "OGA9ENC1"
	encSaltLen   = 16
	encNonceLen  = 12
	encChunkSize = 4 << 20 // 4 MiB plaintext per chunk
)

func chunkNonce(base [encNonceLen]byte, counter uint32) []byte {
	n := make([]byte, encNonceLen)
	copy(n, base[:])
	v := binary.BigEndian.Uint32(n[encNonceLen-4:])
	binary.BigEndian.PutUint32(n[encNonceLen-4:], v^counter)
	return n
}

// Encrypt streams the plaintext tar.gz through chunked AEAD into w.
func Encrypt(w io.Writer, r io.Reader, passphrase string) error {
	if passphrase == "" {
		return errors.New("bundle: passphrase required for credentials export")
	}
	var salt [encSaltLen]byte
	var nonce [encNonceLen]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return err
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := argon2.IDKey([]byte(passphrase), salt[:], 1, 64*1024, 4, 32)
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(encMagic)); err != nil {
		return err
	}
	if _, err := w.Write(salt[:]); err != nil {
		return err
	}
	if _, err := w.Write(nonce[:]); err != nil {
		return err
	}
	buf := make([]byte, encChunkSize)
	var counter uint32
	var lenBuf [4]byte
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			ct := aead.Seal(nil, chunkNonce(nonce, counter), buf[:n], nil)
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
			if _, err := w.Write(lenBuf[:]); err != nil {
				return err
			}
			if _, err := w.Write(ct); err != nil {
				return err
			}
			counter++
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// decryptToTemp streams the chunked ciphertext to a plaintext temp file
// (same backing the raw path uses) and returns its path.
func decryptToTemp(r io.Reader, passphrase string) (string, error) {
	var magic [len(encMagic)]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || string(magic[:]) != encMagic {
		return "", fmt.Errorf("bundle: bad envelope magic")
	}
	var hdr [encSaltLen + encNonceLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", fmt.Errorf("bundle: truncated envelope: %w", err)
	}
	if passphrase == "" {
		return "", errors.New("bundle is encrypted — passphrase required")
	}
	salt, nonce := hdr[:encSaltLen], [encNonceLen]byte{}
	copy(nonce[:], hdr[encSaltLen:])
	key := argon2.IDKey([]byte(passphrase), salt, 1, 64*1024, 4, 32)
	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "oga9exp-dec-*")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	var counter uint32
	var lenBuf [4]byte
	for {
		if _, err := io.ReadFull(r, lenBuf[:]); err == io.EOF {
			break
		} else if err != nil {
			return fail(fmt.Errorf("bundle: truncated chunk header: %w", err))
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n == 0 || n > encChunkSize+16 {
			return fail(errors.New("bundle: corrupt chunk length"))
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(r, ct); err != nil {
			return fail(fmt.Errorf("bundle: truncated chunk: %w", err))
		}
		pt, err := aead.Open(nil, chunkNonce(nonce, counter), ct, nil)
		if err != nil {
			return fail(errors.New("bundle decryption failed — wrong passphrase or corrupt data"))
		}
		if _, err := f.Write(pt); err != nil {
			return fail(err)
		}
		counter++
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	// the spool convention keeps one temp per archive — hand the open
	// file's path over and let Close() remove it.
	path := f.Name()
	f.Close()
	return path, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

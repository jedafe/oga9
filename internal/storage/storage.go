// Ref: oga9-engineering/sections/14-comms-documents.md — blob storage.
// MVP backend: local filesystem under OGA9_UPLOADS_DIR (self-hostable,
// Rule 29 — no external service required). storage_key is a path-safe
// uuid; S3-compatible backends plug in behind this same interface later.
package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Store struct{ dir string }

func New(dir string) *Store { _ = os.MkdirAll(dir, 0o755); return &Store{dir: dir} }

// Put writes the blob; keys are generated uuids — we clean the input
// anyway so a stray ../ can never escape the root.
func (s *Store) Put(ctx context.Context, key string, r io.Reader) error {
	key = filepath.Clean("/" + strings.ReplaceAll(key, "..", ""))
	f, err := os.Create(filepath.Join(s.dir, key))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	key = filepath.Clean("/" + strings.ReplaceAll(key, "..", ""))
	return os.Open(filepath.Join(s.dir, key))
}

func (s *Store) Stat(ctx context.Context, key string) (int64, error) {
	st, err := os.Stat(filepath.Join(s.dir, filepath.Clean("/"+strings.ReplaceAll(key, "..", ""))))
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Remove deletes a key — used when an upload is rejected after the blob
// landed (over-cap, failed validation).
func (s *Store) Remove(ctx context.Context, key string) error {
	key = filepath.Clean("/" + strings.ReplaceAll(key, "..", ""))
	return os.Remove(filepath.Join(s.dir, key))
}

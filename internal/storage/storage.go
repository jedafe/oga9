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

// Store confines every key inside dir via os.Root — traversal segments
// AND symlink escapes can't reach outside the uploads root (Go 1.24+).
type Store struct {
	dir  string
	root *os.Root
}

func New(dir string) *Store {
	_ = os.MkdirAll(dir, 0o750)
	root, _ := os.OpenRoot(dir) // nil only when dir is unusable — ops fail then too
	return &Store{dir: dir, root: root}
}

// cleanKey anchors the key: os.Root rejects ".." itself, but cleaning
// also collapses redundant separators so Stat/Remove hit the same file
// a Put wrote.
func cleanKey(key string) string {
	return strings.TrimPrefix(filepath.Clean("/"+key), "/")
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader) error {
	key = cleanKey(key)
	if s.root == nil {
		return s.putJoined(ctx, key, r)
	}
	if dir := filepath.Dir(key); dir != "." {
		if err := s.root.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	f, err := s.root.Create(key)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, r)
	return err
}

func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	key = cleanKey(key)
	if s.root == nil {
		return os.Open(filepath.Join(s.dir, key)) // #nosec G304 -- fallback when os.Root unavailable; cleanKey strips traversal
	}
	return s.root.Open(key)
}

func (s *Store) Stat(ctx context.Context, key string) (int64, error) {
	key = cleanKey(key)
	var st os.FileInfo
	var err error
	if s.root == nil {
		st, err = os.Stat(filepath.Join(s.dir, key))
	} else {
		st, err = s.root.Stat(key)
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Remove deletes a key — used when an upload is rejected after the blob
// landed (over-cap, failed validation).
func (s *Store) Remove(ctx context.Context, key string) error {
	key = cleanKey(key)
	if s.root == nil {
		return os.Remove(filepath.Join(s.dir, key))
	}
	return s.root.Remove(key)
}

// putJoined is the fallback for a dir os.Root couldn't open — the join
// still stays under s.dir because cleanKey strips traversal.
func (s *Store) putJoined(ctx context.Context, key string, r io.Reader) error {
	dst := filepath.Join(s.dir, key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	f, err := os.Create(dst) // #nosec G304 -- fallback when os.Root unavailable; cleanKey strips traversal
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, r)
	return err
}

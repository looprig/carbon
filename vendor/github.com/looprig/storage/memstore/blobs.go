package memstore

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/looprig/storage"
)

// blobStore is the in-memory Blobs backing type: content-addressed immutable
// byte objects guarded by a single RWMutex. Each key maps to bytes the store
// owns; a Put of byte-identical content to an existing key is a no-op success,
// while different content is rejected with a *BlobConflictError leaving the
// original unchanged.
//
// As an in-process oracle it performs no blocking I/O beyond draining the
// caller's reader and does NOT honor ctx cancellation; each method's ctx
// parameter exists solely to satisfy the storage.Blobs contract.
type blobStore struct {
	mu    sync.RWMutex
	blobs map[string][]byte
}

// newBlobStore returns an empty blobStore ready for use.
func newBlobStore() *blobStore {
	return &blobStore{blobs: make(map[string][]byte)}
}

// Compile-time proof that *blobStore honors the Blobs contract.
var _ storage.Blobs = (*blobStore)(nil)

// Put reads r to completion and stores the bytes at key. It validates the key
// first (*InvalidNameError). If key already holds byte-identical content the Put
// is a no-op success; if it holds different content Put returns a
// *BlobConflictError and leaves the original object unchanged. The reader is
// drained before the lock is taken, so I/O never blocks other operations; the
// resulting slice is owned by the store (io.ReadAll allocates it fresh).
func (s *blobStore) Put(ctx context.Context, key string, r io.Reader) error {
	if err := storage.ValidateName(key); err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err // propagate the caller's reader failure verbatim
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.blobs[key]; ok {
		if bytes.Equal(existing, data) {
			return nil // byte-identical: content-addressed no-op
		}
		return &storage.BlobConflictError{Key: key}
	}
	s.blobs[key] = data
	return nil
}

// Get returns an independent io.ReadCloser over a copy of the bytes at key. It
// validates the key first (*InvalidNameError); an absent key yields a
// *BlobNotFoundError. Each Get produces a fresh reader over its own copy, so a
// caller reading, closing, or mutating the bytes cannot affect stored data or
// any other reader.
func (s *blobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := storage.ValidateName(key); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, ok := s.blobs[key]
	if !ok {
		return nil, &storage.BlobNotFoundError{Key: key}
	}
	out := make([]byte, len(data))
	copy(out, data)
	return io.NopCloser(bytes.NewReader(out)), nil
}

// Delete removes key; it validates the key first (*InvalidNameError) and is
// idempotent, so deleting an absent key succeeds. After Delete the key is free:
// a fresh Put of any content stores it without a lingering conflict.
func (s *blobStore) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateName(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blobs, key)
	return nil
}

// List returns the keys whose string has prefix, lexicographically ascending and
// duplicate-free (map keys are unique by construction). An empty prefix returns
// all keys. Per the contract, List does NOT validate prefix — a partial-segment
// prefix like "blobs/" is a substring filter, not a valid name.
func (s *blobStore) List(ctx context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []string
	for k := range s.blobs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

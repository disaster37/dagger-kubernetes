package repository

import (
	"context"
)

// MetaStore reads/writes arbitrary key/value pairs via Raft. Drop-in
// replacement for the SQLite-backed MetaStore used by cmd/api/main.go for the
// JWT secret and token-encryption key.
type MetaStore struct {
	store *RaftStore
}

// NewMetaStore returns a MetaStore backed by store.
func NewMetaStore(store *RaftStore) *MetaStore {
	return &MetaStore{store: store}
}

// Get returns the value for key. A missing key yields domain.ErrNotFound.
func (m *MetaStore) Get(ctx context.Context, key string) (string, error) {
	return m.store.fsmRead().readMeta(key)
}

// Set upserts the value for key.
func (m *MetaStore) Set(ctx context.Context, key, value string) error {
	return m.store.applyCtx(ctx, kindSetMeta, cmdSetMeta{Key: key, Value: value})
}

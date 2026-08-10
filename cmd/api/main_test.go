package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

func newMetaStore(t *testing.T) *repository.MetaStore {
	t.Helper()
	db, err := repository.OpenSQLite(t.TempDir() + "/jwt.db")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := repository.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return repository.NewMetaStore(db)
}

func TestLoadOrCreateJWTSecretConfiguredOK(t *testing.T) {
	ms := newMetaStore(t)
	secret := strings.Repeat("k", minJWTSecretLen)
	got, err := loadOrCreateJWTSecret(context.Background(), ms, secret, observ.NewTestLogger())
	if err != nil {
		t.Fatalf("loadOrCreateJWTSecret: %v", err)
	}
	if string(got) != secret {
		t.Fatalf("secret = %q, want configured", got)
	}
}

func TestLoadOrCreateJWTSecretTooShortRejected(t *testing.T) {
	ms := newMetaStore(t)
	if _, err := loadOrCreateJWTSecret(context.Background(), ms, "short", observ.NewTestLogger()); err == nil {
		t.Fatal("expected error for short secret (CWE-326)")
	}
}

func TestLoadOrCreateJWTSecretGeneratedAndPersisted(t *testing.T) {
	ms := newMetaStore(t)
	logger := observ.NewTestLogger()

	got, err := loadOrCreateJWTSecret(context.Background(), ms, "", logger)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(got) < minJWTSecretLen {
		t.Fatalf("generated secret too short: %d", len(got))
	}

	// Second call returns the persisted value (stable across restarts).
	again, err := loadOrCreateJWTSecret(context.Background(), ms, "", logger)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("generated secret must be persisted and reused")
	}
}

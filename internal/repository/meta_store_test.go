package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestMetaStoreGetSet(t *testing.T) {
	ms := NewMetaStore(newTestRaftStore(t))
	ctx := context.Background()

	if _, err := ms.Get(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing: %v, want ErrNotFound", err)
	}

	if err := ms.Set(ctx, "k", "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := ms.Get(ctx, "k")
	if err != nil || got != "v1" {
		t.Fatalf("Get = %q err=%v", got, err)
	}

	// Upsert.
	if err := ms.Set(ctx, "k", "v2"); err != nil {
		t.Fatalf("Set upsert: %v", err)
	}
	got, _ = ms.Get(ctx, "k")
	if got != "v2" {
		t.Fatalf("Get after upsert = %q", got)
	}
}

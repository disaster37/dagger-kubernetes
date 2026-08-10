package service

import (
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestResolverFloor(t *testing.T) {
	r, err := NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	v019, _ := domain.Parse("v0.19.0")
	v016, _ := domain.Parse("v0.16.0")

	if !r.IsAllowed(v019) {
		t.Fatal("v0.19.0 should be allowed")
	}
	if r.IsAllowed(v016) {
		t.Fatal("v0.16.0 should not be allowed")
	}
}

func TestResolverAllowlist(t *testing.T) {
	r, err := NewResolver("v0.19.0", []string{"0.21"}, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	v021, _ := domain.Parse("v0.21.4")
	v020, _ := domain.Parse("v0.20.0")

	if !r.IsAllowed(v021) {
		t.Fatal("v0.21.x should be allowed")
	}
	if r.IsAllowed(v020) {
		t.Fatal("v0.20.x should not be allowed")
	}
}

func TestResolveMinimal(t *testing.T) {
	releases := map[string][]string{
		"0.21": {"v0.21.0", "v0.21.1", "v0.21.4"},
	}
	r, _ := NewResolver("v0.19.0", nil, releases)

	v, err := r.ResolveMinimal("0.21")
	if err != nil {
		t.Fatalf("ResolveMinimal: %v", err)
	}
	if v.Patch != 4 {
		t.Fatalf("expected patch 4, got %d", v.Patch)
	}
}

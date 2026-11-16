package service

import (
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestMemoryDeviceAuthStore(t *testing.T) {
	store := NewMemoryDeviceAuthStore()
	a := &domain.DeviceAuthorization{DeviceCode: "dev", UserCode: "ABCD-EFGH", ExpiresAt: time.Now().Add(time.Minute)}
	if err := store.Create(a); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve("ABCD-EFGH", "u1"); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetByDeviceCode("dev")
	if err != nil || !got.Approved || got.UserID != "u1" {
		t.Fatalf("got = %+v err=%v", got, err)
	}
	if err := store.Consume("dev"); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetByDeviceCode("dev")
	if !got.Consumed {
		t.Fatal("expected consumed")
	}
}

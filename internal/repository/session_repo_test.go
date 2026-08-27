package repository

import (
	"context"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// captureSessionSink records the replicated session updates for assertions.
type captureSessionSink struct {
	registered []*domain.Lease
	touched    []struct {
		fp string
		at time.Time
	}
}

func (c *captureSessionSink) ApplySessionRegistered(lease *domain.Lease) {
	c.registered = append(c.registered, lease)
}

func (c *captureSessionSink) ApplySessionTouched(certFP string, at time.Time) {
	c.touched = append(c.touched, struct {
		fp string
		at time.Time
	}{certFP, at})
}

func TestSessionRepoRegisterAndTouchReplicate(t *testing.T) {
	store := newTestRaftStore(t)
	sink := &captureSessionSink{}
	store.SetSessionSink(sink)
	repo := NewSessionRepo(store)
	ctx := context.Background()

	if err := repo.Register(ctx, "fp1", "v0.21.9", "dagger-engine-v0-21-9-0", "instance-1", "trace-1", "user-1", "group-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(sink.registered) != 1 {
		t.Fatalf("sink registered = %d, want 1", len(sink.registered))
	}
	l := sink.registered[0]
	if l.CertFP != "fp1" || l.Version != "v0.21.9" || l.ReplicaPod != "dagger-engine-v0-21-9-0" ||
		l.InstanceID != "instance-1" || l.TraceID != "trace-1" || l.UserID != "user-1" || l.GroupID != "group-1" {
		t.Fatalf("lease = %+v", l)
	}
	if l.LastActivity.IsZero() {
		t.Fatal("LastActivity must be set from the leader clock")
	}
	if l.InFlight != 0 {
		t.Fatalf("InFlight = %d, want 0", l.InFlight)
	}

	if err := repo.Touch(ctx, "fp1"); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if len(sink.touched) != 1 {
		t.Fatalf("sink touched = %d, want 1", len(sink.touched))
	}
	if sink.touched[0].fp != "fp1" {
		t.Fatalf("touched fp = %q, want fp1", sink.touched[0].fp)
	}
	if sink.touched[0].at.IsZero() {
		t.Fatal("touch timestamp must be set")
	}
}

func TestSessionRepoRejectsEmptyFingerprint(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewSessionRepo(store)
	ctx := context.Background()

	if err := repo.Register(ctx, "", "v0.21.9", "pod-0", "i", "t", "u", "g"); err == nil {
		t.Fatal("Register with empty certFP must fail")
	}
	if err := repo.Touch(ctx, ""); err == nil {
		t.Fatal("Touch with empty certFP must fail")
	}
}

func TestSessionRepoClipsFreeFormFields(t *testing.T) {
	store := newTestRaftStore(t)
	sink := &captureSessionSink{}
	store.SetSessionSink(sink)
	repo := NewSessionRepo(store)
	ctx := context.Background()

	long := make([]byte, maxSessionFieldLen+50)
	for i := range long {
		long[i] = 'x'
	}
	if err := repo.Register(ctx, "fp1", "v0.21.9", "pod-0", string(long), string(long), string(long), string(long)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	l := sink.registered[0]
	if len(l.InstanceID) != maxSessionFieldLen || len(l.TraceID) != maxSessionFieldLen ||
		len(l.UserID) != maxSessionFieldLen || len(l.GroupID) != maxSessionFieldLen {
		t.Fatalf("free-form lease fields must be clipped to %d bytes", maxSessionFieldLen)
	}
}

package repository

import (
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// sessionSinkRecorder captures FSM-forwarded session updates.
type sessionSinkRecorder struct {
	registered []*domain.Lease
	touches    []struct {
		fp string
		at time.Time
	}
}

func (r *sessionSinkRecorder) ApplySessionRegistered(lease *domain.Lease) {
	cp := *lease
	r.registered = append(r.registered, &cp)
}

func (r *sessionSinkRecorder) ApplySessionTouched(certFP string, at time.Time) {
	r.touches = append(r.touches, struct {
		fp string
		at time.Time
	}{certFP, at})
}

func TestFSMSessionCommandsForwardToSink(t *testing.T) {
	f := newTestFSM(t)
	sink := &sessionSinkRecorder{}
	f.state.sessionSink = sink

	at := time.Now().UTC().Truncate(time.Second)
	if err := applyCmd(t, f, kindUpsertSession, cmdUpsertSession{
		Lease: domain.Lease{
			CertFP:     "fp1",
			Version:    "v0.21.9",
			ReplicaPod: "dagger-engine-v0-21-9-0",
			TraceID:    "trace-1",
			UserID:     "user-1",
		},
		At: at,
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if len(sink.registered) != 1 {
		t.Fatalf("registered = %d, want 1", len(sink.registered))
	}
	got := sink.registered[0]
	if got.CertFP != "fp1" || got.Version != "v0.21.9" || got.ReplicaPod != "dagger-engine-v0-21-9-0" || got.TraceID != "trace-1" || got.UserID != "user-1" {
		t.Fatalf("lease = %+v", got)
	}
	if !got.LastActivity.Equal(at) {
		t.Fatalf("LastActivity = %v, want %v (leader clock)", got.LastActivity, at)
	}

	if err := applyCmd(t, f, kindTouchSession, cmdTouchSession{CertFP: "fp1", At: at.Add(time.Minute)}); err != nil {
		t.Fatalf("touch session: %v", err)
	}
	if len(sink.touches) != 1 || sink.touches[0].fp != "fp1" || !sink.touches[0].at.Equal(at.Add(time.Minute)) {
		t.Fatalf("touches = %+v", sink.touches)
	}
}

func TestFSMSessionCommandsWithoutSinkAreNoops(t *testing.T) {
	f := newTestFSM(t)
	// No sink wired: the commands must apply without error (e.g. a store
	// constructed before the sink was available).
	if err := applyCmd(t, f, kindUpsertSession, cmdUpsertSession{
		Lease: domain.Lease{CertFP: "fp1"},
		At:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert session without sink: %v", err)
	}
	if err := applyCmd(t, f, kindTouchSession, cmdTouchSession{CertFP: "fp1", At: time.Now().UTC()}); err != nil {
		t.Fatalf("touch session without sink: %v", err)
	}
}

package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type fakeOTLPArchive struct {
	mu       sync.Mutex
	chunks   map[string][]domain.OTLPChunk
	failNext int
	reads    int
}

func newFakeOTLPArchive() *fakeOTLPArchive {
	return &fakeOTLPArchive{chunks: map[string][]domain.OTLPChunk{}}
}

func (a *fakeOTLPArchive) Append(_ context.Context, traceID, signal string, seq uint64, payload []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failNext > 0 {
		a.failNext--
		return errors.New("transient archive failure")
	}
	key := traceID + "/" + signal
	a.chunks[key] = append(a.chunks[key], domain.OTLPChunk{Seq: seq, Payload: append([]byte(nil), payload...)})
	return nil
}

func (a *fakeOTLPArchive) ChunksAfter(_ context.Context, traceID, signal string, fromSeq uint64) ([]domain.OTLPChunk, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	var out []domain.OTLPChunk
	for _, c := range a.chunks[traceID+"/"+signal] {
		if c.Seq >= fromSeq {
			out = append(out, c)
		}
	}
	return out, nil
}

func (a *fakeOTLPArchive) DeleteTrace(context.Context, string) error { return nil }

func TestOTLPStreamLiveReadsAvoidArchive(t *testing.T) {
	archive := newFakeOTLPArchive()
	svc := NewOTLPStreamService(archive, nil, logrus.New())
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		svc.Ingest(ctx, "trace", "traces", []byte{byte(i)})
	}
	chunks, _, err := svc.ChunksFrom(ctx, "trace", "traces", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Seq != uint64(i) {
			t.Fatalf("seq[%d] = %d", i, c.Seq)
		}
	}
	if archive.reads != 0 {
		t.Fatalf("live read made %d archive calls", archive.reads)
	}
}

func TestOTLPStreamFailureRecoveryKeepsSequence(t *testing.T) {
	archive := newFakeOTLPArchive()
	archive.failNext = 1
	svc := NewOTLPStreamService(archive, nil, logrus.New())
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		svc.Ingest(ctx, "trace", "traces", []byte{byte(i)})
	}
	// Force a flush; the first append fails and the pending suffix is retained.
	svc.flush(ctx, "trace", "traces")
	svc.flush(ctx, "trace", "traces")
	chunks, _, err := svc.ChunksFrom(ctx, "trace", "traces", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Seq != uint64(i) {
			t.Fatalf("seq[%d] = %d", i, c.Seq)
		}
	}
}

func TestOTLPStreamColdReadUsesArchive(t *testing.T) {
	archive := newFakeOTLPArchive()
	svc := NewOTLPStreamService(archive, nil, logrus.New())
	ctx := context.Background()
	svc.Ingest(ctx, "trace", "traces", []byte("a"))
	svc.flush(ctx, "trace", "traces")
	// Evict the live buffer to force a cold read.
	svc.mu.Lock()
	delete(svc.buffers, otlpKey("trace", "traces"))
	svc.mu.Unlock()
	chunks, _, err := svc.ChunksFrom(ctx, "trace", "traces", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Seq != 0 {
		t.Fatalf("chunks = %+v", chunks)
	}
	if archive.reads == 0 {
		t.Fatal("cold read must call the archive")
	}
}

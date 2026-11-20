package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	otlpFlushBytes    = 4 << 20 // 4 MiB
	otlpFlushEvery    = 2 * time.Second
	otlpRingMaxChunks = 256
	otlpRingMaxBytes  = 16 << 20 // 16 MiB
	otlpBufferIdleTTL = 30 * time.Second
)

// OTLPStreamService tees raw OTLP payloads into a per-trace live ring buffer
// and micro-batches them to an OTLPArchive (ADR-054).
type OTLPStreamService struct {
	archive   domain.OTLPArchive
	traceMeta domain.TraceMetaRepository
	logger    *logrus.Logger

	mu      sync.Mutex
	buffers map[string]*otlpBuffer
}

type otlpBuffer struct {
	nextSeq      uint64 // assigned monotonically at Ingest under mu
	archivedNext uint64 // lowest Seq not yet confirmed in archive
	pendingBytes int
	lastFlush    time.Time
	pending      []domain.OTLPChunk // awaiting archive.Append
	ring         []domain.OTLPChunk // bounded live ring (max 256 chunks / 16 MiB)
	notify       chan struct{}      // closed & replaced on Ingest to wake live readers immediately
}

// NewOTLPStreamService returns an OTLPStreamService.
func NewOTLPStreamService(archive domain.OTLPArchive, traceMeta domain.TraceMetaRepository, logger *logrus.Logger) *OTLPStreamService {
	return &OTLPStreamService{archive: archive, traceMeta: traceMeta, logger: logger, buffers: make(map[string]*otlpBuffer)}
}

func otlpKey(orgID, traceID, signal string) string {
	return fmt.Sprintf("%s/%s/%s", orgID, traceID, signal)
}

// Ingest buffers one raw OTLP payload for traceID/signal and flushes when the
// size threshold is reached.
func (s *OTLPStreamService) Ingest(ctx context.Context, orgID, traceID, signal string, payload []byte) {
	if s.archive == nil || traceID == "" || len(payload) == 0 {
		return
	}
	key := otlpKey(orgID, traceID, signal)
	s.mu.Lock()
	buf := s.buffers[key]
	if buf == nil {
		buf = &otlpBuffer{lastFlush: time.Now(), notify: make(chan struct{})}
		s.buffers[key] = buf
	}
	seq := buf.nextSeq
	buf.nextSeq++
	chunk := domain.OTLPChunk{Seq: seq, Payload: append([]byte(nil), payload...)}
	buf.pending = append(buf.pending, chunk)
	buf.pendingBytes += len(payload)
	buf.ring = append(buf.ring, chunk)
	for len(buf.ring) > otlpRingMaxChunks || ringBytes(buf.ring) > otlpRingMaxBytes {
		buf.ring = buf.ring[1:]
	}
	close(buf.notify)
	buf.notify = make(chan struct{})
	flush := buf.pendingBytes >= otlpFlushBytes
	s.mu.Unlock()
	if flush {
		s.flush(ctx, orgID, traceID, signal)
	}
}

func ringBytes(ring []domain.OTLPChunk) int {
	total := 0
	for _, c := range ring {
		total += len(c.Payload)
	}
	return total
}

// flush writes buffered chunks to the archive. Successfully appended chunks
// are removed from the buffer; on a transient archive failure the un-appended
// suffix retains its original sequence numbers so the next tick retries the
// exact same (Seq, Payload) idempotently.
func (s *OTLPStreamService) flush(ctx context.Context, orgID, traceID, signal string) {
	key := otlpKey(orgID, traceID, signal)
	s.mu.Lock()
	buf := s.buffers[key]
	if buf == nil || len(buf.pending) == 0 {
		s.mu.Unlock()
		return
	}
	pending := append([]domain.OTLPChunk(nil), buf.pending...)
	buf.lastFlush = time.Now()
	s.mu.Unlock()

	appended := 0
	for i, chunk := range pending {
		if err := s.archive.Append(ctx, orgID, traceID, signal, chunk.Seq, chunk.Payload); err != nil {
			s.logger.WithError(err).WithFields(logrus.Fields{"trace_id": traceID, "signal": signal}).Warn("otlp archive append failed")
			break
		}
		appended = i + 1
	}

	s.mu.Lock()
	buf = s.buffers[key]
	if buf == nil {
		s.mu.Unlock()
		return
	}
	if appended > 0 {
		buf.archivedNext = pending[appended-1].Seq + 1
		trimmed := buf.pending[:0]
		for _, c := range buf.pending {
			if c.Seq >= buf.archivedNext {
				trimmed = append(trimmed, c)
			}
		}
		buf.pending = trimmed
		buf.pendingBytes = 0
		for _, c := range buf.pending {
			buf.pendingBytes += len(c.Payload)
		}
	}
	s.mu.Unlock()
}

// ChunksFrom returns archived chunks with Seq >= fromSeq plus any still-buffered
// live chunks. It never forces an S3 flush on reader polls.
func (s *OTLPStreamService) ChunksFrom(ctx context.Context, orgID, traceID, signal string, fromSeq uint64) ([]domain.OTLPChunk, <-chan struct{}, error) {
	key := otlpKey(orgID, traceID, signal)
	s.mu.Lock()
	buf := s.buffers[key]
	var notify <-chan struct{}
	if buf != nil {
		notify = buf.notify
	}
	live := s.liveChunksLocked(buf, fromSeq)
	s.mu.Unlock()

	if live != nil {
		return live, notify, nil
	}
	if s.archive == nil {
		return nil, notify, nil
	}
	archived, err := s.archive.ChunksAfter(ctx, orgID, traceID, signal, fromSeq)
	if err != nil {
		return nil, notify, err
	}
	s.mu.Lock()
	buf = s.buffers[key]
	if buf != nil {
		notify = buf.notify
		last := fromSeq
		if len(archived) > 0 {
			last = archived[len(archived)-1].Seq + 1
		}
		for _, c := range buf.pending {
			if c.Seq >= last {
				archived = append(archived, c)
			}
		}
	}
	s.mu.Unlock()
	return archived, notify, nil
}

// liveChunksLocked returns the in-memory chunks when the ring/pending buffers
// fully cover fromSeq (0 S3 calls), or nil when a cold read is required.
func (s *OTLPStreamService) liveChunksLocked(buf *otlpBuffer, fromSeq uint64) []domain.OTLPChunk {
	if buf == nil {
		return nil
	}
	ringCovers := len(buf.ring) > 0 && buf.ring[0].Seq <= fromSeq
	pendingCovers := buf.archivedNext <= fromSeq && (len(buf.pending) == 0 || buf.pending[0].Seq <= fromSeq)
	if !ringCovers && !pendingCovers {
		return nil
	}
	out := make([]domain.OTLPChunk, 0, len(buf.ring)+len(buf.pending))
	seen := make(map[uint64]struct{}, len(buf.ring)+len(buf.pending))
	for _, c := range buf.ring {
		if c.Seq >= fromSeq {
			out = append(out, c)
			seen[c.Seq] = struct{}{}
		}
	}
	for _, c := range buf.pending {
		if c.Seq >= fromSeq {
			if _, ok := seen[c.Seq]; ok {
				continue
			}
			out = append(out, c)
		}
	}
	return out
}

// IsTerminal reports whether the trace has reached a terminal status.
func (s *OTLPStreamService) IsTerminal(ctx context.Context, traceID string) bool {
	if s.traceMeta == nil {
		return false
	}
	m, err := s.traceMeta.Get(ctx, traceID)
	if err != nil {
		return false
	}
	return m.Status == "success" || m.Status == "failed"
}

// Start runs a periodic flush loop until ctx is cancelled.
func (s *OTLPStreamService) Start(ctx context.Context) {
	ticker := time.NewTicker(otlpFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flushAll(ctx)
		}
	}
}

func (s *OTLPStreamService) flushAll(ctx context.Context) {
	s.mu.Lock()
	keys := make([]string, 0, len(s.buffers))
	for key, buf := range s.buffers {
		if len(buf.pending) > 0 {
			keys = append(keys, key)
		}
	}
	s.mu.Unlock()
	for _, key := range keys {
		parts := strings.SplitN(key, "/", 3)
		if len(parts) != 3 {
			continue
		}
		s.flush(ctx, parts[0], parts[1], parts[2])
	}
	s.evictIdle(ctx)
}

func (s *OTLPStreamService) evictIdle(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, buf := range s.buffers {
		if len(buf.pending) != 0 || time.Since(buf.lastFlush) <= otlpBufferIdleTTL {
			continue
		}
		idx := strings.LastIndex(key, "/")
		if idx <= 0 || idx == len(key)-1 {
			continue
		}
		if s.IsTerminal(ctx, key[:idx]) {
			delete(s.buffers, key)
		}
	}
}

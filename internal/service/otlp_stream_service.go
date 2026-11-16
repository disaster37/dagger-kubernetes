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
	otlpFlushBytes = 4 << 20 // 4 MiB
	otlpFlushEvery = 2 * time.Second
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
	seq       uint64
	bytes     int
	lastFlush time.Time
	chunks    [][]byte
}

// NewOTLPStreamService returns an OTLPStreamService.
func NewOTLPStreamService(archive domain.OTLPArchive, traceMeta domain.TraceMetaRepository, logger *logrus.Logger) *OTLPStreamService {
	return &OTLPStreamService{archive: archive, traceMeta: traceMeta, logger: logger, buffers: make(map[string]*otlpBuffer)}
}

func otlpKey(traceID, signal string) string {
	return fmt.Sprintf("%s/%s", traceID, signal)
}

// Ingest buffers one raw OTLP payload for traceID/signal and flushes when the
// size or time threshold is reached.
func (s *OTLPStreamService) Ingest(ctx context.Context, traceID, signal string, payload []byte) {
	if s.archive == nil || traceID == "" || len(payload) == 0 {
		return
	}
	key := otlpKey(traceID, signal)
	s.mu.Lock()
	buf := s.buffers[key]
	if buf == nil {
		buf = &otlpBuffer{lastFlush: time.Now()}
		s.buffers[key] = buf
	}
	buf.chunks = append(buf.chunks, append([]byte(nil), payload...))
	buf.bytes += len(payload)
	flush := buf.bytes >= otlpFlushBytes || time.Since(buf.lastFlush) >= otlpFlushEvery
	s.mu.Unlock()
	if flush {
		s.flush(ctx, traceID, signal)
	}
}

// flush writes buffered chunks to the archive.
func (s *OTLPStreamService) flush(ctx context.Context, traceID, signal string) {
	key := otlpKey(traceID, signal)
	s.mu.Lock()
	buf := s.buffers[key]
	if buf == nil || len(buf.chunks) == 0 {
		s.mu.Unlock()
		return
	}
	chunks := buf.chunks
	buf.chunks = nil
	buf.bytes = 0
	buf.lastFlush = time.Now()
	startSeq := buf.seq
	buf.seq += uint64(len(chunks))
	s.mu.Unlock()

	for i, chunk := range chunks {
		if err := s.archive.Append(ctx, traceID, signal, startSeq+uint64(i), chunk); err != nil {
			s.logger.WithError(err).WithFields(logrus.Fields{"trace_id": traceID, "signal": signal}).Warn("otlp archive append failed")
			return
		}
	}
}

// Chunks returns all archived chunks plus any still-buffered live chunks.
func (s *OTLPStreamService) Chunks(ctx context.Context, traceID, signal string) ([][]byte, error) {
	s.flush(ctx, traceID, signal)
	if s.archive == nil {
		return nil, nil
	}
	return s.archive.Chunks(ctx, traceID, signal)
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
		if len(buf.chunks) > 0 {
			keys = append(keys, key)
		}
	}
	s.mu.Unlock()
	for _, key := range keys {
		idx := strings.LastIndex(key, "/")
		if idx <= 0 || idx == len(key)-1 {
			continue
		}
		s.flush(ctx, key[:idx], key[idx+1:])
	}
}

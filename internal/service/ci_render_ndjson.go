package service

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// NDJSONEventSink writes each CIEvent as a single JSON line. It is the wire
// format consumed by ci-integrations/jenkins (and reusable by Drone/GHA).
type NDJSONEventSink struct {
	w     io.Writer
	enc   *json.Encoder
	flush func() error // nil-safe flush for http.Flusher-backed writers
}

// NewNDJSONEventSink returns a sink writing to w. If w implements Flush(), the
// sink flushes after each line so streaming consumers receive events promptly.
func NewNDJSONEventSink(w io.Writer) *NDJSONEventSink {
	s := &NDJSONEventSink{
		w:   w,
		enc: json.NewEncoder(w),
	}
	if f, ok := w.(interface{ Flush() error }); ok {
		s.flush = f.Flush
	}
	return s
}

func (s *NDJSONEventSink) Emit(e *domain.CIEvent) error {
	if err := s.enc.Encode(e); err != nil {
		return fmt.Errorf("encode ci event: %w", err)
	}
	if s.flush != nil {
		if err := s.flush(); err != nil {
			return fmt.Errorf("flush ci event: %w", err)
		}
	}
	return nil
}

func (s *NDJSONEventSink) Flush() error {
	if s.flush != nil {
		if err := s.flush(); err != nil {
			return fmt.Errorf("flush ci event stream: %w", err)
		}
	}
	return nil
}

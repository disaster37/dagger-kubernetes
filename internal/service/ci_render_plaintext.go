package service

import (
	"fmt"
	"io"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// PlaintextEventSink writes CI events as human-readable lines. It is used by
// CI integrations (Jenkins) that need real-time plaintext output from the
// NDJSON event stream without requiring jq or other external tools.
type PlaintextEventSink struct {
	w     io.Writer
	flush func() error
}

// NewPlaintextEventSink returns a sink writing to w. If w implements Flush(),
// the sink flushes after each line so streaming consumers receive output
// promptly.
func NewPlaintextEventSink(w io.Writer) *PlaintextEventSink {
	s := &PlaintextEventSink{w: w}
	if f, ok := w.(interface{ Flush() error }); ok {
		s.flush = f.Flush
	}
	return s
}

func (s *PlaintextEventSink) Emit(e *domain.CIEvent) error {
	var line string
	switch e.Type {
	case domain.CIEventNodeStarted:
		if e.Node != nil {
			line = fmt.Sprintf("\u25b6 %s", e.Node.Name)
		}
	case domain.CIEventNodeFinished:
		if e.Node != nil {
			line = fmt.Sprintf("\u25c0 %s (%s)", e.Node.Name, e.Node.State)
			if e.Error != "" {
				line = fmt.Sprintf("%s: %s", line, e.Error)
			}
		}
	case domain.CIEventLogChunk:
		if e.Log != nil {
			for _, l := range e.Log.Lines {
				if _, err := fmt.Fprintln(s.w, l); err != nil {
					return fmt.Errorf("write plaintext log line: %w", err)
				}
			}
			if s.flush != nil {
				if err := s.flush(); err != nil {
					return fmt.Errorf("flush plaintext log: %w", err)
				}
			}
			return nil
		}
	case domain.CIEventPipelineDone:
		line = fmt.Sprintf("Pipeline: %s", e.Status)
		if e.Error != "" {
			line = fmt.Sprintf("%s (%s)", line, e.Error)
		}
	}
	if line == "" {
		return nil
	}
	if _, err := fmt.Fprintln(s.w, line); err != nil {
		return fmt.Errorf("write plaintext event: %w", err)
	}
	if s.flush != nil {
		if err := s.flush(); err != nil {
			return fmt.Errorf("flush plaintext event: %w", err)
		}
	}
	return nil
}

func (s *PlaintextEventSink) Flush() error {
	if s.flush != nil {
		if err := s.flush(); err != nil {
			return fmt.Errorf("flush plaintext event stream: %w", err)
		}
	}
	return nil
}

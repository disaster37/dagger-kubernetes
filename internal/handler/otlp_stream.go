package handler

import (
	"context"
	"io"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const otlpHeartbeatInterval = 15 * time.Second

// frameSeq converts a chunk counter to the wire sequence number.
func frameSeq(n int) uint64 { return uint64(n) } // #nosec G115 -- chunk count is bounded by memory

// handleOTLPStream serves GET /v1/{traces,logs,metrics}/:traceID as framed
// binary otlpstream (ADR-054).
func (s *Server) handleOTLPStream(signal string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		traceID, ok := s.authorizeTraceRequest(ctx, c)
		if !ok {
			return
		}
		if s.otlpStream == nil {
			writeError(c, consts.StatusNotFound, "otlp stream disabled")
			return
		}
		c.Response.Header.Set("Content-Type", domain.OTLPStreamContentType)
		c.Response.Header.Set("Cache-Control", "no-cache")
		c.Response.Header.Set("Connection", "keep-alive")
		c.Response.ImmediateHeaderFlush = true
		c.SetStatusCode(consts.StatusOK)

		pr, pw := io.Pipe()
		c.SetBodyStream(pr, -1)
		listen := c.Query("listen") != "false"
		go s.streamOTLPFrames(ctx, pw, traceID, signal, listen)
	}
}

// streamOTLPFrames writes archived chunks, then live chunks/heartbeats until
// the trace is terminal or the client disconnects.
func (s *Server) streamOTLPFrames(ctx context.Context, w io.WriteCloser, traceID, signal string, listen bool) {
	defer func() { _ = w.Close() }()
	written := 0
	heartbeat := time.NewTicker(otlpHeartbeatInterval)
	defer heartbeat.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	writeNew := func() bool {
		chunks, err := s.otlpStream.Chunks(ctx, traceID, signal)
		if err != nil {
			_ = domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameError, Payload: []byte(err.Error())})
			return false
		}
		for ; written < len(chunks); written++ {
			if err := domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameData, Seq: frameSeq(written), Payload: chunks[written]}); err != nil {
				return false
			}
		}
		return true
	}

	if !writeNew() {
		return
	}
	if !listen {
		_ = domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameTerminal, Seq: frameSeq(written)})
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			if !writeNew() {
				return
			}
			if s.otlpStream.IsTerminal(ctx, traceID) {
				_ = domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameTerminal, Seq: frameSeq(written)})
				return
			}
		case <-heartbeat.C:
			if err := domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameData, Seq: frameSeq(written)}); err != nil {
				return
			}
		}
	}
}

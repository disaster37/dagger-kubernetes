package handler

import (
	"context"
	"io"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const otlpHeartbeatInterval = 15 * time.Second

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
		nextSeq := uint64(0)
		if afterStr := c.Query("after"); afterStr != "" {
			u, err := strconv.ParseUint(afterStr, 10, 64)
			if err != nil {
				writeError(c, consts.StatusBadRequest, "invalid after cursor")
				return
			}
			nextSeq = u + 1
		}
		c.Response.Header.Set("Content-Type", domain.OTLPStreamContentType)
		c.Response.Header.Set("Cache-Control", "no-cache")
		c.Response.Header.Set("Connection", "keep-alive")
		c.Response.ImmediateHeaderFlush = true
		c.SetStatusCode(consts.StatusOK)

		pr, pw := io.Pipe()
		c.SetBodyStream(pr, -1)
		listen := c.Query("listen") != "false"
		go s.streamOTLPFrames(ctx, pw, traceID, signal, nextSeq, listen)
	}
}

// streamOTLPFrames writes archived chunks, then live chunks/heartbeats until
// the trace is terminal or the client disconnects.
func (s *Server) streamOTLPFrames(ctx context.Context, w io.WriteCloser, traceID, signal string, nextSeq uint64, listen bool) {
	defer func() { _ = w.Close() }()
	heartbeat := time.NewTicker(otlpHeartbeatInterval)
	defer heartbeat.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	writeNew := func() (bool, <-chan struct{}) {
		chunks, notify, err := s.otlpStream.ChunksFrom(ctx, traceID, signal, nextSeq)
		if err != nil {
			_ = domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameError, Payload: []byte(err.Error())})
			return false, nil
		}
		for _, chunk := range chunks {
			if err := domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameData, Seq: chunk.Seq, Payload: chunk.Payload}); err != nil {
				return false, nil
			}
			nextSeq = chunk.Seq + 1
		}
		return true, notify
	}

	ok, notify := writeNew()
	if !ok {
		return
	}
	if !listen {
		_ = domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameTerminal, Seq: nextSeq})
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-notify:
			if ok, notify = writeNew(); !ok {
				return
			}
		case <-poll.C:
			if ok, notify = writeNew(); !ok {
				return
			}
			if s.otlpStream.IsTerminal(ctx, traceID) {
				// Drain one final time so trailing chunks are never truncated.
				if ok, _ = writeNew(); !ok {
					return
				}
				_ = domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameTerminal, Seq: nextSeq})
				return
			}
		case <-heartbeat.C:
			if err := domain.WriteOTLPFrame(w, domain.OTLPFrame{Type: domain.FrameData, Seq: nextSeq}); err != nil {
				return
			}
		}
	}
}

package handler

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleHistoryInfo serves the history stats + GC rules (GET /api/v1/history).
func (s *Server) handleHistoryInfo(ctx context.Context, c *app.RequestContext) {
	if !s.requireAuth(c) {
		return
	}
	if s.historyStats == nil {
		writeError(c, consts.StatusInternalServerError, "history stats unavailable")
		return
	}
	stats, err := s.historyStats.Stats(ctx)
	if err != nil {
		s.logger.WithError(err).Error("history stats failed")
		writeError(c, consts.StatusInternalServerError, "history stats failed")
		return
	}
	writeJSON(c, stats)
}

// handleHistoryPurge purges a single trace (POST /api/v1/history/purge, admin-only).
func (s *Server) handleHistoryPurge(ctx context.Context, c *app.RequestContext) {
	if !s.requireHistoryPurger(c) {
		return
	}
	var req domain.HistoryPurgeRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.TraceID == "" || !domain.ValidTraceID(req.TraceID) {
		writeError(c, consts.StatusBadRequest, "invalid trace_id")
		return
	}
	result, err := s.historyPurger.Purge(ctx, req)
	if err != nil {
		s.writeHistoryPurgeError(c, err)
		return
	}
	writeJSON(c, result)
}

// handleHistoryPurgeAll purges every trace older than history.gc.max_age
// (POST /api/v1/history/purge-all, admin-only).
func (s *Server) handleHistoryPurgeAll(ctx context.Context, c *app.RequestContext) {
	if !s.requireHistoryPurger(c) {
		return
	}
	result, err := s.historyPurger.PurgeAll(ctx)
	if err != nil {
		s.writeHistoryPurgeError(c, err)
		return
	}
	writeJSON(c, result)
}

// requireHistoryPurger gates the purge handlers behind a non-nil purger.
func (s *Server) requireHistoryPurger(c *app.RequestContext) bool {
	if s.historyPurger == nil {
		writeError(c, consts.StatusInternalServerError, "history purge unavailable")
		return false
	}
	return true
}

func (s *Server) writeHistoryPurgeError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, domain.ErrValidation):
		writeError(c, consts.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrHistoryPurgeRunning):
		writeError(c, consts.StatusConflict, "history purge already in progress")
	default:
		s.logger.WithError(err).Error("history purge failed")
		writeError(c, consts.StatusInternalServerError, "history purge failed")
	}
}

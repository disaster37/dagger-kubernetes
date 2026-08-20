package handler

import (
	"context"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// handleTracesList returns a scoped list of trace metadata. Admins see all
// (with an optional ?group_id= filter, including "unassigned"); users see
// traces in their groups plus their own unassigned traces.
func (s *Server) handleTracesList(_ context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(c)
	if !ok {
		return
	}

	f := domain.TraceFilter{Limit: clampLimit(c.Query("limit"))}
	if id.IsAdmin() {
		// Admin may narrow with ?group_id= (repeatable); the "unassigned"
		// keyword selects only traces without a group. Without a filter
		// admins see everything.
		f.IncludeUnassigned = true
		for _, gid := range c.QueryArgs().PeekAll("group_id") {
			if string(gid) == "unassigned" {
				f.UnassignedOnly = true
				continue
			}
			f.GroupIDs = append(f.GroupIDs, string(gid))
		}
	} else {
		f.GroupIDs = id.GroupIDs
		f.UserID = id.UserID
	}

	res, err := s.traceMeta.List(context.Background(), f)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, res)
}

// handleTracesDetail returns a single trace's span tree (Tempo), enriched with
// the persisted trace_meta row. Gated by authorizeTrace (owner/member/admin;
// unknown meta -> admin-only).
func (s *Server) handleTracesDetail(_ context.Context, c *app.RequestContext) {
	traceID, ok := s.authorizeTraceRequest(c)
	if !ok {
		return
	}
	trace, err := s.traces.GetTrace(traceID)
	if err != nil {
		writeError(c, consts.StatusNotFound, "trace not found")
		return
	}

	// The Tempo reconstruction derives Version/CIProvider/CIRepo from the root
	// span's span-level attributes, which the Dagger CLI leaves empty (those are
	// resource-level attributes dropped during reconstruction). The SQLite
	// trace_meta row populated at provision/ingest time is authoritative, so
	// merge it in best-effort and only fill fields Tempo left empty.
	if meta, err := s.traceMeta.Get(context.Background(), traceID); err == nil {
		if trace.Version == "" {
			trace.Version = meta.Version
		}
		if trace.CIProvider == "" {
			trace.CIProvider = meta.CIProvider
		}
		if trace.CIRepo == "" {
			trace.CIRepo = meta.CIRepo
			if trace.CIRepo == "" {
				trace.CIRepo = meta.ProjectName
			}
		}
		// The Tempo reconstruction may still report "running" while the root
		// span's finish record has not been indexed yet. Only promote to a
		// final status when trace_meta captured one at ingest; a stale
		// "running" (or empty) meta value must never downgrade a final status.
		if (trace.Status == "running" || trace.Status == "") && (meta.Status == "success" || meta.Status == "failed") {
			trace.Status = meta.Status
		}
		if trace.DurationMS == 0 && meta.DurationMS != 0 {
			trace.DurationMS = meta.DurationMS
			trace.Duration = time.Duration(meta.DurationMS) * time.Millisecond
		}
		// User attribution: UserID from trace_meta, Username joined best-effort
		// from the users table (empty for legacy/anonymous or deleted users).
		trace.UserID = meta.UserID
		if meta.UserID != "" {
			if u, err := s.users.Get(context.Background(), meta.UserID); err == nil {
				trace.Username = u.Username
			} else {
				s.logger.WithError(err).WithField("user_id", meta.UserID).Debug("trace user lookup failed")
			}
		}
	} else {
		s.logger.WithError(err).WithField("trace_id", traceID).Debug("trace_meta enrichment failed")
	}

	// Best-effort self-hosted pipeline view URL: a misconfigured base does not
	// break trace detail; the url field is simply omitted.
	if u, ok := s.pipelineViewURL(traceID); ok {
		trace.URL = u
	}

	writeJSON(c, trace)
}

// handleTracesLogs returns a trace's logs (Loki). Gated by authorizeTrace.
func (s *Server) handleTracesLogs(_ context.Context, c *app.RequestContext) {
	traceID, ok := s.authorizeTraceRequest(c)
	if !ok {
		return
	}
	s.queryAndWriteTraceLogs(traceID, c)
}

// handleTracesLive streams live span updates for a trace over Server-Sent
// Events using Hertz's native SSE writer. Gated by authorizeTrace. EventSource
// clients cannot set headers, so this is the only route that also accepts the
// ?token= query param (D14).
func (s *Server) handleTracesLive(ctx context.Context, c *app.RequestContext) {
	if !s.requireAuthWithQueryFallback(c) {
		return
	}
	traceID, ok := traceIDParam(c)
	if !ok {
		return
	}
	if !s.authorizeTrace(c, traceID) {
		return
	}

	c.SetStatusCode(consts.StatusOK)
	c.Response.Header.Set("Content-Type", "text/event-stream")
	c.Response.Header.Set("Cache-Control", "no-cache")
	c.Response.Header.Set("Connection", "keep-alive")

	client := repository.NewLiveClient(c, traceID)

	s.liveHub.Subscribe(traceID, client)

	select {
	case <-ctx.Done():
	case <-client.Done():
	}
	s.liveHub.Unsubscribe(traceID, client)
}

// authorizeTraceRequest resolves the identity, extracts the :traceID path
// parameter, and enforces trace visibility. On any failure it writes the
// response and returns false.
func (s *Server) authorizeTraceRequest(c *app.RequestContext) (string, bool) {
	if !s.requireAuth(c) {
		return "", false
	}
	traceID, ok := traceIDParam(c)
	if !ok {
		return "", false
	}
	if !s.authorizeTrace(c, traceID) {
		return "", false
	}
	return traceID, true
}

// traceIDParam extracts the :traceID path parameter; when it is missing it
// writes a 400 response and returns false.
func traceIDParam(c *app.RequestContext) (string, bool) {
	traceID := strings.TrimSuffix(c.Param("traceID"), "/")
	if traceID == "" {
		writeError(c, consts.StatusBadRequest, "missing trace ID")
		return "", false
	}
	return traceID, true
}

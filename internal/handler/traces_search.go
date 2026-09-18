package handler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

const (
	defaultTraceSearchLimit = 500
	maxTraceSearchLimit     = 2000
)

// handleTracesSearch serves a subtree-scoped, text-filtered, paginated log
// search for a trace. Gated by authorizeTraceRequest (owner/member/admin).
func (s *Server) handleTracesSearch(_ context.Context, c *app.RequestContext) {
	traceID, ok := s.authorizeTraceRequest(c)
	if !ok {
		return
	}

	mode := domain.LogSearchMode(c.Query("mode"))
	if mode == "" {
		mode = domain.LogSearchContains
	}
	if mode != domain.LogSearchContains && mode != domain.LogSearchRegex {
		writeError(c, consts.StatusBadRequest, "invalid mode")
		return
	}

	cursor, err := parseSearchCursor(c.Query("cursor"))
	if err != nil {
		writeError(c, consts.StatusBadRequest, "invalid cursor")
		return
	}

	focus := strings.TrimSpace(c.Query("span_id"))
	var spanIDs []string
	if focus != "" {
		trace, err := s.traces.GetTrace(traceID)
		if err != nil {
			writeError(c, consts.StatusNotFound, "trace not found")
			return
		}
		ids, found := service.LevelSpanIDs(trace.RootSpan, focus)
		if !found {
			writeError(c, consts.StatusNotFound, "span not found")
			return
		}
		spanIDs = ids
	}

	start, end := searchWindow(c)
	page, err := s.logs.SearchTraceLogs(context.Background(), traceID, domain.LogSearchRequest{
		SpanIDs:       spanIDs,
		Query:         c.Query("q"),
		Mode:          mode,
		Start:         start,
		End:           end,
		Limit:         clampSearchLimit(c.Query("limit")),
		Cursor:        cursor,
		IncludeCounts: cursor == 0,
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidRegex) {
			writeError(c, consts.StatusBadRequest, "invalid regex")
			return
		}
		s.logger.WithError(err).WithField("trace_id", traceID).Error("log search failed")
		writeError(c, consts.StatusBadGateway, "log search failed")
		return
	}

	writeJSON(c, page)
}

// clampSearchLimit parses a limit query param, defaulting to
// defaultTraceSearchLimit and capping at maxTraceSearchLimit.
func clampSearchLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultTraceSearchLimit
	}
	if n > maxTraceSearchLimit {
		return maxTraceSearchLimit
	}
	return n
}

// parseSearchCursor parses the forward pagination cursor (unix nanos). An empty
// value means "from the start" (0); a non-integer or negative value is an error.
func parseSearchCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse cursor %q: %w", raw, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("cursor must not be negative")
	}
	return n, nil
}

// searchWindow returns the inclusive [start, end] window for a search request,
// defaulting to the last 24 hours. Optional ?start=/?end= unix-nanos params
// override the defaults.
func searchWindow(c *app.RequestContext) (start, end time.Time) {
	end = time.Now()
	if raw := c.Query("end"); raw != "" {
		if ns, err := strconv.ParseInt(raw, 10, 64); err == nil && ns > 0 {
			end = time.Unix(0, ns)
		}
	}
	start = end.Add(-24 * time.Hour)
	if raw := c.Query("start"); raw != "" {
		if ns, err := strconv.ParseInt(raw, 10, 64); err == nil && ns > 0 {
			start = time.Unix(0, ns)
		}
	}
	return start, end
}

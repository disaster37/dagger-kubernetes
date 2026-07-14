package api

import (
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/disaster/dagger-kubernetes/internal/telemetry"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) handleTracesRoutes(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if strings.HasSuffix(path, "/live") {
		s.handleTracesLive(w, r)
		return
	}
	if strings.HasSuffix(path, "/logs") {
		s.handleTracesLogs(w, r)
		return
	}
	if path == "/api/v1/traces" || path == "/api/v1/traces/" {
		s.handleTracesList(w, r)
		return
	}
	s.handleTracesDetail(w, r)
}

func (s *Server) handleTracesList(w http.ResponseWriter, _ *http.Request) {
	traces := []map[string]string{
		{"trace_id": "example-1", "status": "success", "version": "v0.21.4"},
	}
	writeJSON(w, traces)
}

func (s *Server) handleTracesDetail(w http.ResponseWriter, r *http.Request) {
	traceID := extractTraceID(r.URL.Path)
	if traceID == "" {
		writeError(w, http.StatusBadRequest, "missing trace ID")
		return
	}

	reconstructor := telemetry.NewSpanTreeReconstructor(s.cfg.TempoURL)
	trace, err := reconstructor.GetTrace(traceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "trace not found")
		return
	}

	writeJSON(w, trace)
}

func (s *Server) handleTracesLogs(w http.ResponseWriter, r *http.Request) {
	traceID := extractTraceID(strings.TrimSuffix(r.URL.Path, "/logs"))
	if traceID == "" {
		writeError(w, http.StatusBadRequest, "missing trace ID")
		return
	}
	writeJSON(w, map[string]string{
		"trace_id": traceID,
		"logs":     "",
	})
}

func (s *Server) handleTracesLive(w http.ResponseWriter, r *http.Request) {
	traceID := extractTraceID(strings.TrimSuffix(r.URL.Path, "/live"))
	if traceID == "" {
		writeError(w, http.StatusBadRequest, "missing trace ID")
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("websocket upgrade failed", zap.Error(err))
		return
	}

	client := &telemetry.LiveClient{
		Conn:    conn,
		TraceID: traceID,
		Send:    make(chan []byte, 256),
	}

	s.liveHub.Subscribe(traceID, client)

	go func() {
		defer s.liveHub.Unsubscribe(traceID, client)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				break
			}
		}
	}()
}

func extractTraceID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "traces" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

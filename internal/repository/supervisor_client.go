package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// maxSupervisorResponseBytes caps each supervisor response body so a
// compromised or misbehaving endpoint cannot stream an unbounded JSON document
// into the wrapper's memory (CWE-400). A var (not const) so tests can lower it.
var maxSupervisorResponseBytes int64 = 32 << 20 // 32 MiB

// SupervisorTraceClient reads the reconstructed span tree + logs from the
// supervisor's REST API. Implements domain.TraceSnapshotSource.
type SupervisorTraceClient struct {
	baseURL    string // e.g. https://supv.example.com
	token      string // Bearer token
	httpClient *http.Client
}

var _ domain.TraceSnapshotSource = (*SupervisorTraceClient)(nil)

// NewSupervisorTraceClient builds a client with an explicit timeout so a
// stalled supervisor cannot hang a CI job (mirrors cmd/ci's cliHTTPClient).
func NewSupervisorTraceClient(baseURL, token string, timeout time.Duration) *SupervisorTraceClient {
	return &SupervisorTraceClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// GetTrace fetches GET /api/v1/traces/{id} (Bearer auth) and decodes the
// domain.TraceInfo. The trace ID is validated (hex, bounded length — the same
// rule the supervisor enforces) before it is interpolated into the URL, so a
// malformed ID can never tamper with the request path (CWE-20/CWE-918).
// Non-200 → wrapped error.
func (c *SupervisorTraceClient) GetTrace(traceID string) (*domain.TraceInfo, error) {
	if !domain.ValidTraceID(traceID) {
		return nil, fmt.Errorf("get trace: invalid trace id")
	}
	var out domain.TraceInfo
	if err := c.getJSON(fmt.Sprintf("%s/api/v1/traces/%s", c.baseURL, traceID), &out); err != nil {
		return nil, fmt.Errorf("get trace %s: %w", traceID, err)
	}
	return &out, nil
}

// QueryTraceLogs fetches GET /api/v1/traces/{id}/logs (Bearer auth) and
// decodes []domain.LogEntry. The trace ID is validated before URL
// interpolation (see GetTrace). start/end/limit are passed through as query
// params.
func (c *SupervisorTraceClient) QueryTraceLogs(traceID string, start, end time.Time, limit int) ([]domain.LogEntry, error) {
	if !domain.ValidTraceID(traceID) {
		return nil, fmt.Errorf("query trace logs: invalid trace id")
	}
	params := url.Values{}
	if !start.IsZero() {
		params.Set("start", fmt.Sprintf("%d", start.UnixNano()))
	}
	if !end.IsZero() {
		params.Set("end", fmt.Sprintf("%d", end.UnixNano()))
	}
	if limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", limit))
	}

	u := fmt.Sprintf("%s/api/v1/traces/%s/logs", c.baseURL, traceID)
	if len(params) > 0 {
		u = fmt.Sprintf("%s?%s", u, params.Encode())
	}

	var out struct {
		Entries []domain.LogEntry `json:"entries"`
	}
	if err := c.getJSON(u, &out); err != nil {
		return nil, fmt.Errorf("query trace logs %s: %w", traceID, err)
	}
	return out.Entries, nil
}

// getJSON performs an authenticated GET and decodes the JSON response into out.
func (c *SupervisorTraceClient) getJSON(u string, out any) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSupervisorResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

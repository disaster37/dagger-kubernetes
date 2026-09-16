package repository

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type MetricsClient struct {
	victoriaURL string
	httpClient  *http.Client
}

var _ domain.TraceSeriesDeleter = (*MetricsClient)(nil)

func NewMetricsClient(victoriaURL string) *MetricsClient {
	return &MetricsClient{
		victoriaURL: victoriaURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// DeleteSeries deletes all time series matching the given PromQL match[]
// selectors from VictoriaMetrics. Endpoint:
// POST /api/v1/admin/tsdb/delete_series with match[] form params. Returns
// nil on 204/200. Note: VM deletes whole series (no time range); space is
// reclaimed lazily. Requires -deleteAuthKey unset or matching key.
func (c *MetricsClient) DeleteSeries(ctx context.Context, matchers []string) error {
	if c.victoriaURL == "" {
		return fmt.Errorf("victoria URL not configured")
	}
	if len(matchers) == 0 {
		return nil
	}
	params := url.Values{}
	for _, m := range matchers {
		params.Add("match[]", m)
	}
	deleteURL := fmt.Sprintf("%s/api/v1/admin/tsdb/delete_series?%s", c.victoriaURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deleteURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("victoria delete request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("victoria delete failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("victoria delete returned status %d", resp.StatusCode)
}

// DeleteTraceSeries deletes all metrics tagged with trace_id=<traceID>.
// Assumes the OTel collector promotes trace_id as a metric label named
// "trace_id" (verify on the live cluster).
//
// The traceID is validated (hex-only) and sanitized before being interpolated
// into the PromQL match[] selector, mirroring DeleteTraceLogs. This is
// defense-in-depth against PromQL/CWE-94 selector injection: even though the
// handler/service layers validate the manual-purge path and the sweeper path
// feeds FSM-stored trace IDs, a future loosening of the charset or a new
// unvalidated caller must not be able to break out of {trace_id="..."} and
// delete arbitrary series.
func (c *MetricsClient) DeleteTraceSeries(ctx context.Context, traceID string) error {
	if !hexTraceID.MatchString(traceID) {
		return fmt.Errorf("invalid trace ID format")
	}
	// PromQL label-value escaping is the same as LogQL (escape \ and "). The
	// hex-only validation above makes this a no-op; it is kept symmetric with
	// DeleteTraceLogs so a future loosening of the charset stays bounded.
	sanitized := sanitizeLogQLValue(traceID)
	return c.DeleteSeries(ctx, []string{fmt.Sprintf(`{trace_id="%s"}`, sanitized)})
}

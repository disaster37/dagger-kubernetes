package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// ErrNoData is returned by CacheHitRate when VictoriaMetrics is unconfigured,
// unreachable, or has no BuildKit cache counters yet.
var ErrNoData = errors.New("no data")

// BuildKit cache hit/miss counter PromQL (best-effort). Metric names are
// isolated here so they can be tuned post-deployment without touching logic.
const (
	cacheHitsPromQL   = `sum(increase(buildkit_cache_hits_total[5m]))`
	cacheMissesPromQL = `sum(increase(buildkit_cache_misses_total[5m]))`
)

type MetricsClient struct {
	victoriaURL string
	httpClient  *http.Client
}

var _ domain.CacheMetricsClient = (*MetricsClient)(nil)

func NewMetricsClient(victoriaURL string) *MetricsClient {
	return &MetricsClient{
		victoriaURL: victoriaURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *MetricsClient) doQueryCtx(ctx context.Context, queryURL string) ([]domain.MetricResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("victoria query failed: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("victoria query failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("victoria returned status %d", resp.StatusCode)
	}

	var result struct {
		Data struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("victoria decode failed: %w", err)
	}

	var metrics []domain.MetricResult
	for _, raw := range result.Data.Result {
		var mr domain.MetricResult
		if err := json.Unmarshal(raw, &mr); err != nil {
			continue
		}
		metrics = append(metrics, mr)
	}

	return metrics, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deleteURL, nil)
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

// CacheHitRate queries VictoriaMetrics for cache hit/miss counters over the
// last window. Returns (hit, miss, err); err is ErrNoData-wrapped when
// victoria is unconfigured or has no data.
func (c *MetricsClient) CacheHitRate(ctx context.Context) (hit, miss float64, err error) {
	if c.victoriaURL == "" {
		return 0, 0, fmt.Errorf("%w: victoria URL not configured", ErrNoData)
	}
	h, err := c.instantScalar(ctx, cacheHitsPromQL)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrNoData, err)
	}
	m, err := c.instantScalar(ctx, cacheMissesPromQL)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrNoData, err)
	}
	return h, m, nil
}

// instantScalar runs an instant query expecting a single scalar series.
func (c *MetricsClient) instantScalar(ctx context.Context, query string) (float64, error) {
	params := url.Values{}
	params.Set("query", query)
	queryURL := fmt.Sprintf("%s/api/v1/query?%s", c.victoriaURL, params.Encode())

	results, err := c.doQueryCtx(ctx, queryURL)
	if err != nil {
		return 0, err
	}
	if len(results) == 0 {
		return 0, fmt.Errorf("no series for query")
	}
	r := results[0]
	if len(r.Value) < 2 {
		return 0, fmt.Errorf("empty instant value")
	}
	s, ok := r.Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("instant value is not a string")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse instant value: %w", err)
	}
	return f, nil
}

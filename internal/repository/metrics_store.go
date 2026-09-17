package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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

// promRangeResponse is the Prometheus/VictoriaMetrics query_range response
// envelope. Values are [unixSeconds, "value"] pairs.
type promRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][]interface{}   `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// QueryRange runs a PromQL query_range against VictoriaMetrics and returns the
// points summed across all matched series (engine-fleet aggregate), sorted by
// timestamp. An empty result is not an error.
func (c *MetricsClient) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]domain.MetricPoint, error) {
	if c.victoriaURL == "" {
		return nil, fmt.Errorf("victoria URL not configured")
	}
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}
	if step <= 0 {
		step = time.Second
	}

	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))
	params.Set("step", strconv.FormatInt(int64(step.Seconds()), 10))
	queryURL := fmt.Sprintf("%s/api/v1/query_range?%s", c.victoriaURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("victoria query_range request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("victoria query_range failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("victoria query_range returned status %d", resp.StatusCode)
	}

	var decoded promRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode victoria query_range response: %w", err)
	}

	sums := make(map[int64]float64)
	for _, series := range decoded.Data.Result {
		for _, pair := range series.Values {
			if len(pair) != 2 {
				continue
			}
			ts, ok := pair[0].(float64)
			if !ok {
				continue
			}
			raw, ok := pair[1].(string)
			if !ok {
				continue
			}
			val, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				continue
			}
			// PromQL rate()/aggregations can legitimately yield NaN/Inf (e.g.
			// a single sample in the window). Those are not JSON-encodable and
			// would make writeJSON panic, so drop them (CWE-400/robustness).
			if math.IsNaN(val) || math.IsInf(val, 0) {
				continue
			}
			sums[int64(ts)] += val
		}
	}

	points := make([]domain.MetricPoint, 0, len(sums))
	for ts, val := range sums {
		points = append(points, domain.MetricPoint{T: ts, V: val})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].T < points[j].T })
	return points, nil
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

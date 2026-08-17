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

func NewMetricsClient(victoriaURL string) *MetricsClient {
	return &MetricsClient{
		victoriaURL: victoriaURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *MetricsClient) InstantQuery(query string) ([]domain.MetricResult, error) {
	if c.victoriaURL == "" {
		return nil, fmt.Errorf("victoria URL not configured")
	}

	params := url.Values{}
	params.Set("query", query)

	queryURL := fmt.Sprintf("%s/api/v1/query?%s", c.victoriaURL, params.Encode())

	return c.doQuery(queryURL)
}

func (c *MetricsClient) RangeQuery(query string, start, end time.Time, step time.Duration) ([]domain.MetricResult, error) {
	if c.victoriaURL == "" {
		return nil, fmt.Errorf("victoria URL not configured")
	}

	params := url.Values{}
	params.Set("query", query)
	params.Set("start", fmt.Sprintf("%d", start.Unix()))
	params.Set("end", fmt.Sprintf("%d", end.Unix()))
	params.Set("step", fmt.Sprintf("%ds", int(step.Seconds())))

	queryURL := fmt.Sprintf("%s/api/v1/query_range?%s", c.victoriaURL, params.Encode())

	return c.doQuery(queryURL)
}

func (c *MetricsClient) doQuery(queryURL string) ([]domain.MetricResult, error) {
	return c.doQueryCtx(context.Background(), queryURL)
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

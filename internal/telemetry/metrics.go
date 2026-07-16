package telemetry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type MetricResult struct {
	Metric map[string]string `json:"metric"`
	Values [][]interface{}   `json:"values"`
	Value  []interface{}     `json:"value"`
}

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

func (c *MetricsClient) InstantQuery(query string) ([]MetricResult, error) {
	if c.victoriaURL == "" {
		return nil, fmt.Errorf("victoria URL not configured")
	}

	params := url.Values{}
	params.Set("query", query)

	queryURL := fmt.Sprintf("%s/api/v1/query?%s", c.victoriaURL, params.Encode())

	return c.doQuery(queryURL)
}

func (c *MetricsClient) RangeQuery(query string, start, end time.Time, step time.Duration) ([]MetricResult, error) {
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

func (c *MetricsClient) doQuery(queryURL string) ([]MetricResult, error) {
	resp, err := c.httpClient.Get(queryURL)
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

	var metrics []MetricResult
	for _, raw := range result.Data.Result {
		var mr MetricResult
		if err := json.Unmarshal(raw, &mr); err != nil {
			continue
		}
		metrics = append(metrics, mr)
	}

	return metrics, nil
}

package telemetry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
}

type LogsClient struct {
	lokiURL    string
	httpClient *http.Client
}

func NewLogsClient(lokiURL string) *LogsClient {
	return &LogsClient{
		lokiURL: lokiURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *LogsClient) QueryTraceLogs(traceID string, start, end time.Time, limit int) ([]LogEntry, error) {
	if !hexTraceID.MatchString(traceID) {
		return nil, fmt.Errorf("invalid trace ID format")
	}
	if c.lokiURL == "" {
		return nil, fmt.Errorf("loki URL not configured")
	}

	if limit <= 0 {
		limit = 1000
	}

	sanitized := sanitizeLogQLValue(traceID)

	params := url.Values{}
	params.Set("query", fmt.Sprintf(`{trace_id="%s"}`, sanitized))
	params.Set("start", fmt.Sprintf("%d", start.UnixNano()))
	params.Set("end", fmt.Sprintf("%d", end.UnixNano()))
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("direction", "forward")

	queryURL := fmt.Sprintf("%s/loki/api/v1/query_range?%s", c.lokiURL, params.Encode())

	resp, err := c.httpClient.Get(queryURL)
	if err != nil {
		return nil, fmt.Errorf("loki query failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("loki returned status %d", resp.StatusCode)
	}

	var result struct {
		Data struct {
			Result []struct {
				Values [][]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("loki decode failed: %w", err)
	}

	var entries []LogEntry
	for _, stream := range result.Data.Result {
		for _, v := range stream.Values {
			if len(v) < 2 {
				continue
			}
			ts, err := parseNanos(v[0])
			if err != nil {
				continue
			}
			entries = append(entries, LogEntry{
				Timestamp: ts,
				Line:      v[1],
			})
		}
	}

	return entries, nil
}

func parseNanos(s string) (time.Time, error) {
	var ns int64
	if _, err := fmt.Sscanf(s, "%d", &ns); err != nil {
		return time.Time{}, fmt.Errorf("parse nanos %q: %w", s, err)
	}
	return time.Unix(0, ns), nil
}

func sanitizeLogQLValue(v string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`{`, `\{`,
		`}`, `\}`,
		"\n", `\n`,
	)
	return replacer.Replace(v)
}

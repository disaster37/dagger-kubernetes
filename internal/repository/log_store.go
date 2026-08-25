package repository

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type LogsClient struct {
	lokiURL    string
	httpClient *http.Client
}

var _ domain.LogRepository = (*LogsClient)(nil)

func NewLogsClient(lokiURL string) *LogsClient {
	return &LogsClient{
		lokiURL: lokiURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *LogsClient) QueryTraceLogs(traceID string, start, end time.Time, limit int) ([]domain.LogEntry, error) {
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
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("loki decode failed: %w", err)
	}

	var entries []domain.LogEntry
	for _, stream := range result.Data.Result {
		// The collector promotes the span ID to a Loki stream label as a hex
		// string; Tempo exposes span IDs as base64, so normalise here so the
		// frontend can match logs to spans by string equality.
		spanID := normalizeSpanID(stream.Stream["span_id"])
		for _, v := range stream.Values {
			if len(v) < 2 {
				continue
			}
			ts, err := parseNanos(v[0])
			if err != nil {
				continue
			}
			entries = append(entries, domain.LogEntry{
				Timestamp: ts,
				Line:      v[1],
				SpanID:    spanID,
			})
		}
	}

	return entries, nil
}

// DeleteTraceLogs requests deletion of all log streams for traceID from Loki.
// Endpoint: POST /loki/api/v1/delete with query={trace_id="<id>"}&start=<unix>&end=<unix>.
// Requires Loki compactor with deletion enabled. Returns nil on 204.
func (c *LogsClient) DeleteTraceLogs(ctx context.Context, traceID string) error {
	if !hexTraceID.MatchString(traceID) {
		return fmt.Errorf("invalid trace ID format")
	}
	if c.lokiURL == "" {
		return fmt.Errorf("loki URL not configured")
	}
	sanitized := sanitizeLogQLValue(traceID)
	// Delete all-time logs for this trace_id: start=0, end=now.
	params := url.Values{}
	params.Set("query", fmt.Sprintf(`{trace_id="%s"}`, sanitized))
	params.Set("start", "0")
	params.Set("end", fmt.Sprintf("%d", time.Now().Unix()))
	deleteURL := fmt.Sprintf("%s/loki/api/v1/delete?%s", c.lokiURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deleteURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("loki delete request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("loki delete failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("loki delete returned status %d", resp.StatusCode)
}

// normalizeSpanID converts a span ID label into the base64 form used by the
// Tempo trace API. The collector promotes span IDs as 16-char hex strings
// (OTTL `span_id.string`); Tempo returns 8-byte span IDs base64-encoded, so a
// hex label is re-encoded as base64. Any other value (already-base64, or a
// non-span value) is passed through unchanged.
func normalizeSpanID(label string) string {
	if len(label) != 16 || !isHexString(label) {
		return label
	}
	raw, err := hex.DecodeString(label)
	if err != nil {
		return label
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func isHexString(s string) bool {
	for _, c := range s {
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

func isHexDigit(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func parseNanos(s string) (time.Time, error) {
	ns, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse nanos %q: %w", s, err)
	}
	return time.Unix(0, ns), nil
}

// logQLReplacer escapes characters that would break a LogQL label value.
var logQLReplacer = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	`{`, `\{`,
	`}`, `\}`,
	"\n", `\n`,
)

func sanitizeLogQLValue(v string) string {
	return logQLReplacer.Replace(v)
}

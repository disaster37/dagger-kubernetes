package repository

import (
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
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
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

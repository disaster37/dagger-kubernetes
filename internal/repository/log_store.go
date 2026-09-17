package repository

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// Search batching bounds. logSearchBatch is the raw Loki page size per internal
// query_range; logSearchMaxScan caps the total raw entries examined per
// SearchTraceLogs call so a broad query cannot scan an unbounded stream
// (CWE-400).
const (
	logSearchBatch   = 1000
	logSearchMaxScan = 5000
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

// SearchTraceLogs returns one page of trace logs filtered by the request's
// span set and text match, ascending by timestamp, strictly after req.Cursor.
// It pages Loki forward in bounded batches and returns a Next cursor for the
// following page (0 when Loki is exhausted).
//
//nolint:gocritic // req is passed by value to satisfy domain.LogRepository.
func (c *LogsClient) SearchTraceLogs(ctx context.Context, traceID string, req domain.LogSearchRequest) (domain.LogSearchPage, error) {
	if !hexTraceID.MatchString(traceID) {
		return domain.LogSearchPage{}, fmt.Errorf("invalid trace ID format")
	}
	if c.lokiURL == "" {
		return domain.LogSearchPage{}, fmt.Errorf("loki URL not configured")
	}

	var re *regexp.Regexp
	if req.Mode == domain.LogSearchRegex && req.Query != "" {
		compiled, err := regexp.Compile(req.Query)
		if err != nil {
			return domain.LogSearchPage{}, fmt.Errorf("%w: %v", domain.ErrInvalidRegex, err)
		}
		re = compiled
	}

	limit := req.Limit
	if limit <= 0 {
		limit = logSearchBatch
	}

	spanSet := make(map[string]struct{}, len(req.SpanIDs))
	for _, id := range req.SpanIDs {
		spanSet[id] = struct{}{}
	}

	cursor := req.Cursor
	if cursor == 0 {
		cursor = req.Start.UnixNano()
	}

	var matches []domain.LogEntry
	scanned := 0
	exhausted := false
	var lastRawTs int64

	for scanned < logSearchMaxScan {
		raw, err := c.queryRawLogs(ctx, traceID, cursor, req.End, logSearchBatch)
		if err != nil {
			return domain.LogSearchPage{}, err
		}
		if len(raw) == 0 {
			exhausted = true
			break
		}

		// The per-stream append in queryRawLogs is not globally sorted; sort
		// ascending (stable) so the cursor advances monotonically.
		sort.SliceStable(raw, func(i, j int) bool {
			return raw[i].Timestamp.Before(raw[j].Timestamp)
		})

		matches, lastRawTs = appendMatches(matches, raw, spanSet, &req, re, limit)
		scanned += len(raw)

		if len(matches) >= limit {
			// Stopped early because the page is full: more matches may follow,
			// so return a cursor even when this raw batch was short.
			break
		}
		if len(raw) < logSearchBatch {
			exhausted = true
			break
		}
		cursor = lastRawTs + 1
	}

	page := domain.LogSearchPage{Entries: matches}
	if !exhausted {
		page.Next = lastRawTs + 1
	}
	return page, nil
}

// appendMatches filters one sorted raw batch by span set + text match, appending
// the matches to dst and returning the timestamp of the last raw entry examined
// (the cursor anchor). It stops early once dst reaches limit.
func appendMatches(dst, raw []domain.LogEntry, spanSet map[string]struct{}, req *domain.LogSearchRequest, re *regexp.Regexp, limit int) (out []domain.LogEntry, lastRawTs int64) {
	out = dst
	for _, entry := range raw {
		lastRawTs = entry.Timestamp.UnixNano()
		if len(spanSet) > 0 {
			if _, ok := spanSet[entry.SpanID]; !ok {
				continue
			}
		}
		if !matchLogLine(entry.Line, req.Query, req.Mode, re) {
			continue
		}
		out = append(out, entry)
		if len(out) >= limit {
			break
		}
	}
	return out, lastRawTs
}

// queryRawLogs runs one forward Loki query_range for traceID over
// [startNanos, end] and returns the decoded entries (unsorted).
func (c *LogsClient) queryRawLogs(ctx context.Context, traceID string, startNanos int64, end time.Time, limit int) ([]domain.LogEntry, error) {
	sanitized := sanitizeLogQLValue(traceID)

	params := url.Values{}
	params.Set("query", fmt.Sprintf(`{trace_id="%s"}`, sanitized))
	params.Set("start", fmt.Sprintf("%d", startNanos))
	params.Set("end", fmt.Sprintf("%d", end.UnixNano()))
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("direction", "forward")

	queryURL := fmt.Sprintf("%s/loki/api/v1/query_range?%s", c.lokiURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("loki search request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
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

// matchLogLine reports whether line matches query under mode. re is precompiled
// for regex mode; nil for contains mode. An empty query matches every line.
func matchLogLine(line, query string, mode domain.LogSearchMode, re *regexp.Regexp) bool {
	if query == "" {
		return true
	}
	if mode == domain.LogSearchRegex {
		return re != nil && re.MatchString(line)
	}
	return strings.Contains(line, query)
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

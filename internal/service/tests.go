package service

import (
	"strconv"
	"strings"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// testAttrKeys centralizes the span attribute keys that carry per-test facts,
// in priority order (first present wins). The names come from the OTel test
// semantic conventions emitted by Dagger's test integrations
// (`test.case.name`, `test.suite.name`, `test.case.result.status`) and the
// engine's check attributes (`dagger.io/check.name`, `dagger.io/check.passed`);
// a future rename is a one-line change here. Duration is read from the span
// itself unless a millisecond attribute is present.
var testAttrKeys = struct {
	name     []string
	suite    []string
	file     []string
	status   []string
	duration []string
}{
	name:   []string{"test.case.name", "dagger.io/test.name", "dagger.io/check.name"},
	suite:  []string{"test.suite.name", "dagger.io/test.suite"},
	file:   []string{"test.case.file", "test.file.path", "code.filepath"},
	status: []string{"test.case.result.status", "dagger.io/check.passed", "test.status"},
	// Only explicit millisecond values are trusted; anything else falls back to
	// the span duration.
	duration: []string{"test.case.duration_ms", "dagger.io/test.duration_ms"},
}

// DeriveTests walks the span tree rooted at node, attaches a TestResult to each
// span that carries a test-name attribute, and returns the flattened list in
// tree order. It is a pure transform: safe to call on every trace read, and a
// nil root yields no tests.
func DeriveTests(root *domain.SpanNode) []domain.TestResult {
	if root == nil {
		return nil
	}
	var out []domain.TestResult
	seen := make(map[string]bool)
	var walk func(n *domain.SpanNode)
	walk = func(n *domain.SpanNode) {
		if n == nil {
			return
		}
		if n.SpanID != "" {
			if seen[n.SpanID] {
				return
			}
			seen[n.SpanID] = true
		}
		if tr, ok := testResultFromSpan(n); ok {
			n.Test = &tr
			out = append(out, tr)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	return out
}

// testResultFromSpan builds a TestResult from one span. It reports false when
// the span is not a test (no test-name attribute).
func testResultFromSpan(n *domain.SpanNode) (domain.TestResult, bool) {
	name := strings.TrimSpace(firstAttr(n.Attributes, testAttrKeys.name))
	if name == "" {
		return domain.TestResult{}, false
	}
	tr := domain.TestResult{
		Name:       name,
		Suite:      strings.TrimSpace(firstAttr(n.Attributes, testAttrKeys.suite)),
		File:       strings.TrimSpace(firstAttr(n.Attributes, testAttrKeys.file)),
		Status:     mapTestStatus(firstAttr(n.Attributes, testAttrKeys.status), n.Status),
		DurationMS: spanDurationMS(n),
		SpanID:     n.SpanID,
	}
	return tr, true
}

// mapTestStatus normalizes the OTel test-case status (pass/fail/skip) and the
// engine's boolean check attribute, falling back to the span status.
func mapTestStatus(raw, spanStatus string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pass", "passed", "true", "ok":
		return "passed"
	case "fail", "failed", "false", "error":
		return "failed"
	case "skip", "skipped":
		return "skipped"
	}
	switch spanStatus {
	case "success":
		return "passed"
	case "failed":
		return "failed"
	default:
		return "running"
	}
}

// spanDurationMS prefers an explicit millisecond attribute over the span's own
// duration.
func spanDurationMS(n *domain.SpanNode) int64 {
	if raw := firstAttr(n.Attributes, testAttrKeys.duration); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms >= 0 {
			return ms
		}
	}
	return n.DurationMS
}

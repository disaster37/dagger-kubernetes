package domain

import (
	"fmt"
	"net/url"
	"regexp"
)

// pipelineViewPath is the UI route for a single trace (ui/src/router/index.ts).
const pipelineViewPath = "/pipelines/"

// traceIDRe bounds trace IDs reflected into URLs (mirrors handler.validTraceID).
var pipelineTraceIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// PipelineViewURL builds the self-hosted pipeline-view URL for a trace.
// base must be an absolute http(s) URL; its trailing slash is trimmed.
// traceID must be non-empty and match the safe charset.
// Returns a wrapped error on invalid input (never a partially-built URL).
func PipelineViewURL(base, traceID string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("pipeline url base is empty: %w", ErrValidation)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse pipeline url base: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("pipeline url base must be http(s): %s: %w", base, ErrValidation)
	}
	if u.Host == "" {
		return "", fmt.Errorf("pipeline url base has no host: %s: %w", base, ErrValidation)
	}
	// Reject userinfo (https://user:pass@host): it is silently dropped when
	// building the link, so an operator who configures it would believe
	// credentials are sent to the UI when they are not. No legitimate
	// pipeline-view base URL carries userinfo (CWE-20 defense-in-depth).
	if u.User != nil {
		return "", fmt.Errorf("pipeline url base must not contain userinfo: %s: %w", base, ErrValidation)
	}
	if traceID == "" || !pipelineTraceIDRe.MatchString(traceID) {
		return "", fmt.Errorf("invalid trace id: %q: %w", traceID, ErrValidation)
	}
	return fmt.Sprintf("%s://%s%s%s", u.Scheme, u.Host, pipelineViewPath, traceID), nil
}

// ResolvePipelineBase returns the effective pipeline-view base URL:
// pipelineURL when non-empty, else publicURL. Used by config-load validation
// and the CI wrapper.
func ResolvePipelineBase(publicURL, pipelineURL string) string {
	if pipelineURL != "" {
		return pipelineURL
	}
	return publicURL
}

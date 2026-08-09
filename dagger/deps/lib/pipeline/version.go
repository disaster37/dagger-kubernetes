package pipeline

import (
	"fmt"
	"strings"
)

// VersionStrategy describes how the release version is computed from CI context at runtime.
type VersionStrategy struct {
	BranchPattern    string // default "0.0.0-rc.{build}"
	PRPattern        string // default "0.0.0-pr.{pr}.{build}"
	TagPattern       string // default "{tag}"
	PrereleaseSuffix string // optional; appended to a bumped base version
}

// VersionContext is the CI runtime context.
type VersionContext struct {
	Event    string // "push" | "pull_request" | "tag" | "release"
	Build    int
	PRNumber int
	Tag      string
	Branch   string
}

// ComputeVersion resolves a strategy against a concrete context.
func ComputeVersion(s VersionStrategy, ctx VersionContext) (string, error) {
	pattern := ""
	switch ctx.Event {
	case "push", "release":
		// release uses the same pattern as push (branch-based)
		pattern = s.BranchPattern
	case "pull_request":
		pattern = s.PRPattern
	case "tag":
		pattern = s.TagPattern
	default:
		return "", fmt.Errorf("unknown event: %s", ctx.Event)
	}

	result := pattern
	result = strings.ReplaceAll(result, "{build}", fmt.Sprintf("%d", ctx.Build))
	result = strings.ReplaceAll(result, "{pr}", fmt.Sprintf("%d", ctx.PRNumber))
	result = strings.ReplaceAll(result, "{tag}", ctx.Tag)
	result = strings.ReplaceAll(result, "{branch}", ctx.Branch)

	// PrereleaseSuffix is informational; the renderer/host applies it.
	// ComputeVersion just returns the raw pattern result.
	if s.PrereleaseSuffix != "" {
		result = result + s.PrereleaseSuffix
	}

	return result, nil
}

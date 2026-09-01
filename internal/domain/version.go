package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Version struct {
	Major int
	Minor int
	Patch int
	Raw   string
}

type VersionResolver interface {
	IsAllowed(v *Version) bool
	ResolveMinimal(raw string) (*Version, error)
	Floor() *Version
	AllReleases() []*Version
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?$`)

func Parse(raw string) (*Version, error) {
	raw = strings.TrimPrefix(raw, "v")
	matches := versionRe.FindStringSubmatch(raw)
	if matches == nil {
		return nil, fmt.Errorf("invalid version format: %s", raw)
	}

	major, _ := strconv.Atoi(matches[1])
	minor, _ := strconv.Atoi(matches[2])
	// matches[3] is empty for "MAJOR.MINOR" input; Atoi("") yields 0.
	patch, _ := strconv.Atoi(matches[3])

	return &Version{Major: major, Minor: minor, Patch: patch, Raw: fmt.Sprintf("v%d.%d.%d", major, minor, patch)}, nil
}

// fullVersionRe matches a complete MAJOR.MINOR.PATCH release version (the
// optional leading "v" and all three numeric components are required).
var fullVersionRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// IsFullVersion reports whether raw is a complete release version with all
// three components (e.g. "v0.21.0"), as opposed to a partial "MAJOR.MINOR"
// ("0.21") that Parse also accepts with an implicit patch of 0. Callers that
// require a full release (download endpoints) use this to reject partials
// without also rejecting legitimate patch-0 releases.
func IsFullVersion(raw string) bool {
	return fullVersionRe.MatchString(raw)
}

func (v *Version) Compare(other *Version) int {
	if v.Major != other.Major {
		return v.Major - other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor - other.Minor
	}
	return v.Patch - other.Patch
}

func (v *Version) MinorKey() string {
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

func (v *Version) String() string {
	return v.Raw
}

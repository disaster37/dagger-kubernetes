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

func (v *Version) Slug() string {
	return strings.ReplaceAll(v.Raw, ".", "-")
}

func (v *Version) CacheRefTag() string {
	return v.Slug()
}

func (v *Version) String() string {
	return v.Raw
}

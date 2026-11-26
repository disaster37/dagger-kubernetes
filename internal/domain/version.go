package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a SemVer 2.0 version. PreRelease and Build carry the optional
// "-<pre-release>" and "+<build>" segments (e.g. v1.0.0-beta.27).
type Version struct {
	Major      int
	Minor      int
	Patch      int
	PreRelease string
	Build      string
	Raw        string
}

type VersionResolver interface {
	IsAllowed(v *Version) bool
	ResolveMinimal(raw string) (*Version, error)
	Floor() *Version
	AllReleases() []*Version
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$`)

// Parse parses a full or partial (MAJOR.MINOR) SemVer 2.0 version. A partial
// version has an implicit patch of 0. Pre-release and build metadata are
// preserved in Raw and exposed via PreRelease/Build.
func Parse(raw string) (*Version, error) {
	matches := versionRe.FindStringSubmatch(strings.TrimPrefix(raw, "v"))
	if matches == nil {
		return nil, fmt.Errorf("invalid version format: %s", raw)
	}

	major, _ := strconv.Atoi(matches[1])
	minor, _ := strconv.Atoi(matches[2])
	// matches[3] is empty for "MAJOR.MINOR" input; Atoi("") yields 0.
	patch, _ := strconv.Atoi(matches[3])

	v := &Version{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		PreRelease: matches[4],
		Build:      matches[5],
	}
	v.Raw = v.canonical()
	return v, nil
}

// canonical renders the v-prefixed canonical form used on the wire.
func (v *Version) canonical() string {
	raw := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.PreRelease != "" {
		raw = fmt.Sprintf("%s-%s", raw, v.PreRelease)
	}
	if v.Build != "" {
		raw = fmt.Sprintf("%s+%s", raw, v.Build)
	}
	return raw
}

// fullVersionRe matches a complete MAJOR.MINOR.PATCH release version, with an
// optional pre-release and build metadata segment. The leading "v" and all
// three numeric components are required.
var fullVersionRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// IsFullVersion reports whether raw is a complete release version with all
// three components (e.g. "v0.21.0" or "v1.0.0-beta.27"), as opposed to a
// partial "MAJOR.MINOR" ("0.21") that Parse also accepts with an implicit
// patch of 0. Callers that require a full release (download endpoints) use
// this to reject partials without also rejecting legitimate patch-0 releases.
func IsFullVersion(raw string) bool {
	return fullVersionRe.MatchString(raw)
}

// Compare implements SemVer 2.0 precedence: numeric components first, then
// pre-release (a version with a pre-release has lower precedence than the
// same version without), then build metadata is ignored.
func (v *Version) Compare(other *Version) int {
	if c := compareInt(v.Major, other.Major); c != 0 {
		return c
	}
	if c := compareInt(v.Minor, other.Minor); c != 0 {
		return c
	}
	if c := compareInt(v.Patch, other.Patch); c != 0 {
		return c
	}
	return comparePreRelease(v.PreRelease, other.PreRelease)
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePreRelease compares two pre-release strings per SemVer 2.0 §11.
func comparePreRelease(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1 // no pre-release > any pre-release
	}
	if b == "" {
		return -1
	}
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if c := compareIdentifier(aParts[i], bParts[i]); c != 0 {
			return c
		}
	}
	return compareInt(len(aParts), len(bParts))
}

func compareIdentifier(a, b string) int {
	aNum, aErr := strconv.Atoi(a)
	bNum, bErr := strconv.Atoi(b)
	switch {
	case aErr == nil && bErr == nil:
		return compareInt(aNum, bNum)
	case aErr == nil:
		return -1 // numeric identifiers have lower precedence
	case bErr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func (v *Version) MinorKey() string {
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

// IsPreRelease reports whether the version carries a pre-release segment.
func (v *Version) IsPreRelease() bool {
	return v.PreRelease != ""
}

// Slug returns a DNS-safe identifier for the version, e.g.
// "v1.0.0-beta.27" -> "v1-0-0-beta-27".
func (v *Version) Slug() string {
	return VersionSlug(v.Raw)
}

func (v *Version) String() string {
	return v.Raw
}

// VersionSlug converts a version string into a DNS-safe slug by replacing
// dots and build-metadata separators with hyphens. Pre-release hyphens are
// preserved.
func VersionSlug(version string) string {
	replacer := strings.NewReplacer(".", "-", "+", "-")
	return replacer.Replace(version)
}

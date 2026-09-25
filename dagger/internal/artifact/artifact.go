// Package artifact validates version strings and derives deterministic
// release-artifact filenames for the local Dagger module. It is stdlib-only so
// it can be unit-tested without a Dagger session.
package artifact

import (
	"fmt"
	"regexp"
	"strings"
)

// versionRe allows only filesystem/shell-safe version strings: it must start
// with an alphanumeric character and contain only letters, digits, dots,
// underscores and hyphens. Path separators, whitespace and control characters
// are rejected by construction.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// maxVersionLen bounds the version so the derived filename stays well inside
// typical filesystem component limits.
const maxVersionLen = 128

// ValidateVersion reports whether version is safe to embed in an artifact
// filename: non-empty, <=128 chars, no path separators, no "..", no leading
// dot/hyphen, no whitespace or control characters.
//
// It never panics and never silently truncates; every rejection returns an
// error naming the offending input (path traversal / shell injection, CWE-78
// and CWE-22, are blocked before the version reaches a path or a shell).
func ValidateVersion(version string) error {
	if version == "" {
		return fmt.Errorf("version must not be empty")
	}
	if len(version) > maxVersionLen {
		return fmt.Errorf("version %q exceeds %d characters", version, maxVersionLen)
	}
	if !versionRe.MatchString(version) {
		return fmt.Errorf("version %q must start with an alphanumeric character and contain only letters, digits, '.', '_', '-'", version)
	}
	if strings.Contains(version, "..") {
		return fmt.Errorf("version %q must not contain %q", version, "..")
	}
	return nil
}

// Filename returns the deterministic release-artifact filename for version
// (e.g. "jenkins-libs-v0.1.0.tar.gz"), validating version first.
func Filename(version string) (string, error) {
	if err := ValidateVersion(version); err != nil {
		return "", err
	}
	return fmt.Sprintf("jenkins-libs-%s.tar.gz", version), nil
}

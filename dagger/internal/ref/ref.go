// Package ref validates container image reference parts (tag, registry, image)
// for the dagger-kubernetes publish function. It is pure (stdlib only) so it
// can be unit-tested without a running Dagger engine. It does NOT compose the
// final reference — the image dependency builds and publishes it.
package ref

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// OCI distribution tag: [A-Za-z0-9_][A-Za-z0-9._-]{0,127} ("dev", "v0.1.0", "latest", git SHA).
	tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	// Bare registry host, optionally host:port (e.g. "ghcr.io", "localhost:5000").
	registryRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
	// Repository path owner/name[/sub...], lowercase (GHCR lowercases), components
	// separated by ".", "_", or "-".
	imageRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
)

// ValidateTag reports whether tag is a valid container image tag.
func ValidateTag(tag string) error {
	if tag == "" {
		return fmt.Errorf("tag must not be empty")
	}
	if !tagRe.MatchString(tag) {
		return fmt.Errorf("invalid image tag %q: must match [A-Za-z0-9_][A-Za-z0-9._-]{0,127}", tag)
	}
	return nil
}

// ValidateRegistry reports whether registry is a bare host[:port] with no scheme,
// path, or whitespace.
func ValidateRegistry(registry string) error {
	if registry == "" {
		return fmt.Errorf("registry must not be empty")
	}
	if strings.Contains(registry, "://") || strings.ContainsAny(registry, "/ \t\n") {
		return fmt.Errorf("invalid registry %q: must be a bare host[:port] (no scheme, path, or whitespace)", registry)
	}
	if !registryRe.MatchString(registry) {
		return fmt.Errorf("invalid registry %q", registry)
	}
	return nil
}

// ValidateImage reports whether image is a repository path of the form
// owner/name[/sub/...] with no scheme and no leading/trailing slash.
func ValidateImage(image string) error {
	if image == "" {
		return fmt.Errorf("image must not be empty")
	}
	if strings.Contains(image, "://") || strings.HasPrefix(image, "/") || strings.HasSuffix(image, "/") {
		return fmt.Errorf("invalid image %q: must be owner/name without scheme or leading/trailing slash", image)
	}
	if !imageRe.MatchString(image) {
		return fmt.Errorf("invalid image %q: must be lowercase owner/name[/sub...]", image)
	}
	return nil
}

// Validate reports the first invalid part among registry, image, and tag, or
// nil if all three are valid. It is the fail-fast gate Publish calls before
// delegating to the image dependency (which composes the final reference).
func Validate(registry, image, tag string) error {
	if err := ValidateRegistry(registry); err != nil {
		return err
	}
	if err := ValidateImage(image); err != nil {
		return err
	}
	if err := ValidateTag(tag); err != nil {
		return err
	}
	return nil
}

package fleet

import (
	"fmt"
	"strings"
)

func versionSlug(version string) string {
	return strings.ReplaceAll(version, ".", "-")
}

func stsName(version string) string {
	return fmt.Sprintf("dagger-engine-%s", versionSlug(version))
}

func podName(version string, ordinal int) string {
	return fmt.Sprintf("dagger-engine-%s-%d", versionSlug(version), ordinal)
}

func serviceName(version string) string {
	return fmt.Sprintf("dagger-engine-%s", versionSlug(version))
}

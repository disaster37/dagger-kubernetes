#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART_DIR="${ROOT}/deploy/helm/dagger-kubernetes"
CHART_YAML="${CHART_DIR}/Chart.yaml"
README="${CHART_DIR}/README.md"

VERSION="${1:-}"
if [ -z "${VERSION}" ]; then
  VERSION=$(grep '^appVersion:' "${CHART_YAML}" | awk '{print $2}' | tr -d '"')
fi

if [ -z "${VERSION}" ]; then
  echo "error: could not determine version from Chart.yaml or argument" >&2
  exit 1
fi

echo "Updating Helm README version references to ${VERSION}"

# Update the install command version placeholder
sed -i \
  "s/--version [^ ]*/--version ${VERSION}/" \
  "${README}"

# Insert/update a "Latest version" badge line after the description line
MARKER="<!-- version-marker -->"

if grep -q "${MARKER}" "${README}"; then
  sed -i "/${MARKER}/{n;s/^.*\$/   [^1]: Latest released version: \`${VERSION}\`/}" "${README}"
else
  # Insert marker + version line after the description paragraph (line 4)
  sed -i "4a ${MARKER}\n[^1]: Latest released version: \`${VERSION}\`\n" "${README}"
fi

echo "Done. Updated to version ${VERSION}"

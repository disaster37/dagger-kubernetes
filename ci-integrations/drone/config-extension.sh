#!/usr/bin/env bash
set -euo pipefail

DAGGER_CACHE_SERVER="${PLUGIN_SERVER_URL:-${DAGGER_CACHE_SERVER:-}}"
DAGGER_CACHE_TOKEN="${PLUGIN_TOKEN:-${DAGGER_CACHE_TOKEN:-}}"
DAGGER_CACHE_UI="${PLUGIN_UI_URL:-${DAGGER_CACHE_UI:-$DAGGER_CACHE_SERVER}}"
DAGGER_TAG="${PLUGIN_VERSION:-${DAGGER_TAG:-}}"

if [ -z "$DAGGER_CACHE_SERVER" ] || [ -z "$DAGGER_CACHE_TOKEN" ]; then
  echo "Error: server_url and token required" >&2
  exit 1
fi

export DAGGER_CLOUD_URL="$DAGGER_CACHE_SERVER"
export DAGGER_CLOUD_TOKEN="$DAGGER_CACHE_TOKEN"
export _EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self

if [ -n "$DAGGER_TAG" ]; then
  export _EXPERIMENTAL_DAGGER_TAG="$DAGGER_TAG"
fi

INPUT_FILE="${PLUGIN_DRONE_YML:-.drone.yml}"
OUTPUT_FILE="${PLUGIN_OUTPUT:-.drone.dagger.yml}"

if [ -f "$INPUT_FILE" ]; then
  cp "$INPUT_FILE" "$OUTPUT_FILE"
  cat >> "$OUTPUT_FILE" << 'YAML'

steps:
  - name: dagger-cache-summary
    image: alpine:3
    commands:
      - echo "Dagger Cache Pipeline View: $${DAGGER_CACHE_UI}/traces/latest"
    environment:
      DAGGER_CACHE_UI:
        from_secret: dagger_cache_ui
YAML
fi

exec dagger "$@"

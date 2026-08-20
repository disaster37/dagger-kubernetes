#!/usr/bin/env bash
set -euo pipefail

DAGGER_KUBERNETES_SERVER="${PLUGIN_SERVER_URL:-${DAGGER_KUBERNETES_SERVER:-}}"
DAGGER_KUBERNETES_TOKEN="${PLUGIN_TOKEN:-${DAGGER_KUBERNETES_TOKEN:-}}"
DAGGER_KUBERNETES_UI="${PLUGIN_UI_URL:-${DAGGER_KUBERNETES_UI:-$DAGGER_KUBERNETES_SERVER}}"
DAGGER_TAG="${PLUGIN_VERSION:-${DAGGER_TAG:-}}"

if [ -z "$DAGGER_KUBERNETES_SERVER" ] || [ -z "$DAGGER_KUBERNETES_TOKEN" ]; then
  echo "Error: server_url and token required" >&2
  exit 1
fi

export DAGGER_CLOUD_URL="$DAGGER_KUBERNETES_SERVER"
export DAGGER_CLOUD_TOKEN="$DAGGER_KUBERNETES_TOKEN"
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
  - name: dagger-kubernetes-summary
    image: alpine:3
    commands:
      - echo "Dagger Kubernetes Pipeline View: $${DAGGER_KUBERNETES_UI}/traces/latest"
    environment:
      DAGGER_KUBERNETES_UI:
        from_secret: dagger_kubernetes_ui
YAML
fi

exec dagger "$@"

#!/usr/bin/env bash
set -euo pipefail

DAGGER_KUBERNETES_SERVER="${PLUGIN_SERVER_URL:-${DAGGER_KUBERNETES_SERVER:-}}"
DAGGER_KUBERNETES_TOKEN="${PLUGIN_TOKEN:-${DAGGER_KUBERNETES_TOKEN:-}}"
DAGGER_KUBERNETES_UI="${PLUGIN_UI_URL:-${DAGGER_KUBERNETES_UI:-$DAGGER_KUBERNETES_SERVER}}"
DAGGER_TAG="${PLUGIN_VERSION:-${DAGGER_TAG:-}}"

# On-the-fly Dagger CLI provisioning (needs curl + tar). Disable with
# PLUGIN_CLI=false (or DAGGER_KUBERNETES_CLI=false).
DAGGER_KUBERNETES_CLI="${PLUGIN_CLI:-${DAGGER_KUBERNETES_CLI:-true}}"
DAGGER_KUBERNETES_CLI_VERSION="${PLUGIN_CLI_VERSION:-${DAGGER_KUBERNETES_CLI_VERSION:-}}"
DAGGER_KUBERNETES_CLI_OS="${PLUGIN_CLI_OS:-${DAGGER_KUBERNETES_CLI_OS:-linux}}"
DAGGER_KUBERNETES_CLI_ARCH="${PLUGIN_CLI_ARCH:-${DAGGER_KUBERNETES_CLI_ARCH:-amd64}}"

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

if [ "$DAGGER_KUBERNETES_CLI" != "false" ] && [ "$DAGGER_KUBERNETES_CLI" != "0" ]; then
  BIN_DIR="$(mktemp -d)"
  trap 'rm -rf "$BIN_DIR"' EXIT

  if [ -n "$DAGGER_KUBERNETES_CLI_VERSION" ]; then
    DOWNLOAD_URL="${DAGGER_KUBERNETES_SERVER}/api/v1/cli/${DAGGER_KUBERNETES_CLI_VERSION}?os=${DAGGER_KUBERNETES_CLI_OS}&arch=${DAGGER_KUBERNETES_CLI_ARCH}"
  else
    LATEST_URL="${DAGGER_KUBERNETES_SERVER}/api/v1/cli/versions/latest?os=${DAGGER_KUBERNETES_CLI_OS}&arch=${DAGGER_KUBERNETES_CLI_ARCH}"
    LATEST_JSON="$(curl -fsS -H "Authorization: Bearer ${DAGGER_KUBERNETES_TOKEN}" "$LATEST_URL")"
    DOWNLOAD_URL="$(printf '%s' "$LATEST_JSON" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')"
  fi

  curl -fsS -H "Authorization: Bearer ${DAGGER_KUBERNETES_TOKEN}" "$DOWNLOAD_URL" | tar xz -C "$BIN_DIR"
  chmod +x "$BIN_DIR/dagger"
  export PATH="$BIN_DIR:$PATH"
  echo "[dagger-kubernetes] Provisioned Dagger CLI (${DAGGER_KUBERNETES_CLI_VERSION:-latest}) at $BIN_DIR" >&2
fi

exec dagger "$@"

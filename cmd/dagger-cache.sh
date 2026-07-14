#!/usr/bin/env bash
set -euo pipefail

DAGGER_CACHE_SERVER="${DAGGER_CACHE_SERVER:-https://supv.example.com}"
DAGGER_CACHE_UI="${DAGGER_CACHE_UI:-https://ui.supv.example.com}"
CACHE_REGISTRY="${CACHE_REGISTRY:-cache.reg/dagger-cache}"

if [ -z "${DAGGER_CLOUD_TOKEN:-}" ]; then
  echo "Error: DAGGER_CLOUD_TOKEN not set" >&2
  exit 1
fi

export DAGGER_CLOUD_URL="$DAGGER_CACHE_SERVER"
export _EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self

DAGGER_TAG="${DAGGER_TAG:-}"
if [ -z "$DAGGER_TAG" ]; then
  DAGGER_TAG="${_EXPERIMENTAL_DAGGER_TAG:-}"
fi

if [ -n "$DAGGER_TAG" ]; then
  export _EXPERIMENTAL_DAGGER_TAG="$DAGGER_TAG"

  VSLUG=$(echo "$DAGGER_TAG" | sed 's/\./-/g' | sed 's/^v//')
  CACHE_REF="${CACHE_REGISTRY}:V${VSLUG}"

  export _EXPERIMENTAL_DAGGER_CACHE_CONFIG="type=registry,ref=${CACHE_REF},mode=max"
fi

echo "Dagger Cache: $DAGGER_CACHE_SERVER (version: ${DAGGER_TAG:-auto})" >&2

TEMP_LOG=$(mktemp)

set +e
dagger "$@" 2>&1 | tee "$TEMP_LOG"
EXIT_CODE=$?
set -e

TRACE_URL=$(grep -oP 'https://dagger\.cloud/\S+/traces/[a-f0-9]+' "$TEMP_LOG" 2>/dev/null | head -1 || true)
TRACE_ID=""

if [ -n "$TRACE_URL" ]; then
  TRACE_ID=$(echo "$TRACE_URL" | grep -oP '[a-f0-9]{32,}$' || true)
fi

if [ -z "$TRACE_ID" ]; then
  TRACE_ID=$(grep -oP 'trace[_-]?[iI][dD][:=]\s*"?([a-f0-9]{32,})"' "$TEMP_LOG" 2>/dev/null | grep -oP '[a-f0-9]{32,}' | head -1 || true)
fi

if [ -n "$TRACE_ID" ]; then
  echo "" >&2
  echo "┌─────────────────────────────────────────────────────────┐" >&2
  echo "│  Pipeline View: $DAGGER_CACHE_UI/traces/$TRACE_ID" >&2
  echo "└─────────────────────────────────────────────────────────┘" >&2
  echo "TRACE_URL=$DAGGER_CACHE_UI/traces/$TRACE_ID" >> "$TEMP_LOG"
fi

rm -f "$TEMP_LOG"
exit $EXIT_CODE

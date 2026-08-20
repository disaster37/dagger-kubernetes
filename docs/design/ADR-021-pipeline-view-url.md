# ADR-021: Self-hosted pipeline view URL (replace dagger.cloud in client output)

- **Status:** accepted (amended 2026-08-20: `server.pipeline_url` removed)
- **Date:** 2026-08-19
- **Deciders:** dagger-kubernetes maintainers

## Context

When a developer runs the Dagger CLI against this self-hosted platform, the
pipeline-view / trace link surfaced in the CLI output should point at **this
platform's** web UI (`https://dagger.home.webcenter.fr/pipelines/<traceID>`)
instead of `https://dagger.cloud/<org>/traces/<traceID>`.

Verified against the upstream Dagger `main` branch (`engine/telemetry/url.go`):

```go
url = fmt.Sprintf("https://dagger.cloud/%s/traces/%s", orgName, trace.SpanContextFromContext(ctx).TraceID().String())
```

- The host `https://dagger.cloud/` is a **hardcoded string literal**, not
  derived from `DAGGER_CLOUD_URL`.
- `orgName` comes from `auth.CurrentOrgName()` (parsed from a
  `dag_<org>_<secret>` token, or `~/.config/dagger/org`).
- The stock CLI consults **no** server response field, header, or endpoint to
  decide the trace URL (`POST /v1/engines` → `EngineSpec` has no trace-URL
  field; `POST /v1/traces` is standard OTLP whose body is ignored for URL
  purposes).

The platform therefore **cannot**, with the unmodified stock `dagger` CLI,
change what the CLI itself prints. The only client this repository controls is
the **`dagger-kubernetes-ci` wrapper** (`cmd/ci/main.go`), which wraps `dagger`,
extracts the traceID from stderr, and prints a self-hosted link.

A secondary bug existed in that wrapper: it constructed `<uiURL>/traces/<id>`,
but the Vue UI router (`ui/src/router/index.ts`) uses `/pipelines/:id`, so the
printed link landed on the SPA not-found route.

## Decision

Deliver the self-hosted URL via the **wrapper** plus a **dedicated platform
endpoint**, with a single shared helper for URL construction.

### 1. Wrapper (`dagger-kubernetes-ci`) — correct path + config-driven base

- Use the canonical UI path `/pipelines/<traceID>`.
- Derive the URL base with precedence `--ui-url` > `server.public_url`
  (config) > `--server`. `--ui-url`/`--server` remain as backward-compatible
  overrides.
- Add a `--config` flag (default `config.app.yaml`) and load config via
  `config.Load` (which already skips a missing file).

### 2. Platform endpoint

`GET /api/v1/traces/:traceID/url` returns the self-hosted pipeline view URL:

```json
{"trace_id":"<id>","url":"https://<base>/pipelines/<id>"}
```

Auth-gated by the existing `authorizeTraceRequest` (owner/member/admin; unknown
metadata → admin-only). URL derivation does not require the trace to exist in
Tempo, so admins can resolve it for unknown traces.

### 3. `url` field on trace detail

`GET /api/v1/traces/:traceID` (`TraceInfo`) gains an optional `url` field so
the UI and clients fetching the full tree get the URL for free (best-effort; a
misconfigured base logs a WARN and omits the field rather than failing the
request).

### 4. Config key — amended 2026-08-20

~~New optional `server.pipeline_url` (default `""` = fall back to
`server.public_url`), narrowly scoped to "the base URL used to build
pipeline-view links".~~ **Removed**: the base is always `server.public_url`.
A separate pipeline URL was unnecessary — the UI, API and pipeline views are
all served from the same control-plane host, and links always open the same
authenticated UI. The CI wrapper's `--ui-url` flag remains the only override
(for CI environments where the printed link must use a different host than
the API endpoint).

### 5. Centralized helper

`internal/domain/pipeline_url.go` (stdlib only) is the single source of truth:

- `PipelineViewURL(base, traceID) (string, error)`

`config.Load` validates `server.public_url` via `validateServerConfig` (fails
fast at startup when it is not an absolute http(s) URL with a host). The
handler and the wrapper both rely on `PipelineViewURL` (defense-in-depth).

## URL construction rules

- Path template: `<scheme>://<host>/pipelines/<traceID>` (matches the UI route).
- `base` = `server.public_url`.
- Only the scheme + host are taken from `base`; any path/query/fragment is
  dropped — links stay stable behind proxies/TLS-terminating ingresses.
- `traceID` must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` (mirrors
  `handler.validTraceID`).
- Scheme must be `http` or `https`; host must be non-empty. IPv6 hosts, ports,
  localhost/private IPs are all allowed (dev mode).

## Out of scope

- Forking/patching the upstream Dagger CLI.
- Per-request URL overrides via headers/query.
- Deriving the URL from `X-Forwarded-Host` / request `Host` (the configured
  public base is used, so generation is deterministic behind an ingress).

## Consequences

- Users running the bare `dagger` CLI (without the wrapper) still see the
  `dagger.cloud` link — a hard constraint, documented in `docs/README.md`.
- The wrapper's traceID extraction (regex `[a-f0-9]{32,}` on stderr) remains a
  best-effort convenience; the new endpoint lets CI integrations resolve the
  URL from a known traceID without parsing CLI output.
- Switching `/traces/<id>` → `/pipelines/<id>` is a behavior change; any
  consumer that string-matched the old (broken) path needs updating. Low risk:
  the old path 404'd in the UI.
- `server.public_url` must remain set: the supervisor refuses to start
  otherwise, since it cannot derive a pipeline-view URL.

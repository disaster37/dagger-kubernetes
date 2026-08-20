# Plan: Self-hosted pipeline view URL (replace dagger.cloud in client output)

## 1. Problem statement & goal

**User request:** "There are a way for dagger client to display the pipeline view URL instead to display URL relative to dagger.cloud."

**Goal:** When a developer runs the Dagger CLI against this self-hosted platform, the pipeline-view / trace link that gets surfaced should point at **this platform's** web UI (e.g. `https://dagger.home.webcenter.fr/pipelines/<traceID>`) instead of `https://dagger.cloud/<org>/traces/<traceID>`.

## 2. Key research finding (the central constraint)

Verified against the live Dagger `main` branch (`engine/telemetry/url.go`):

```go
func URLForTrace(ctx context.Context) (url string, msg string, ok bool) {
    ...
    url = fmt.Sprintf("https://dagger.cloud/%s/traces/%s", orgName, trace.SpanContextFromContext(ctx).TraceID().String())
    return url, "", true
}
```

- The host `https://dagger.cloud/` is a **hardcoded string literal**. It is NOT derived from `DAGGER_CLOUD_URL`.
- `orgName` comes from `auth.CurrentOrgName()`: parsed from a `dag_<org>_<secret>` token, or read from `~/.config/dagger/org`.
- The stock CLI consults **no** server response field, header, or endpoint to decide the trace URL:
  - `POST /v1/engines` → `EngineSpec{image,url,cert,instance_id,location,org_id,user_id}` — no trace-URL field, none read by the CLI.
  - `POST /v1/traces` (OTLP) → standard OTLP export response; body ignored for URL purposes.
  - No header/endpoint consulted by `URLForTrace`.

**Conclusion:** The platform **cannot**, with the unmodified stock `dagger` CLI, change what the CLI itself prints. The only mechanism this repo controls is the **`dagger-cache-ci` wrapper** (`cmd/ci/main.go`), which wraps `dagger`, extracts the traceID from stderr, and prints a self-hosted `Pipeline View:` link. This plan makes that wrapper correct and config-driven, and adds a platform endpoint so any client (UI, CI, future CLI) can resolve the self-hosted URL for a trace.

**Secondary bug found:** The existing wrapper (`cmd/ci/main.go:86`) constructs `<uiURL>/traces/<traceID>`, but the Vue UI router (`ui/src/router/index.ts:9`) uses `/pipelines/:id`. The printed link currently lands on the SPA not-found route. This plan fixes it.

## 3. Findings — exact files / functions involved

| File | Relevant lines | Role |
|---|---|---|
| `cmd/ci/main.go` | 17 (`traceIDRe`), 47-50 (`--ui-url` default), 86 (`fmt.Sprintf("%s/traces/%s", uiURL, traceID)` — WRONG path), 102-104 (`extractTraceID`) | CI wrapper; prints the self-hosted link. Currently uses wrong UI path and flag-only config. |
| `ui/src/router/index.ts` | 9: `{ path: '/pipelines/:id', name: 'pipeline-detail', ... }` | Canonical UI route for a single pipeline/trace view. |
| `internal/handler/server.go` | 102-111 (`EngineSpecResponse`), 228-240 (`ServerConfig`), 451-454 (route registration), 510-514 (trace routes), 1437-1443 (`traceIDRe`/`validTraceID`) | Hertz server; route table + `ServerConfig`. No `PublicURL`/`PipelineURL` field today. |
| `internal/handler/traces.go` | 53-108 (`handleTracesDetail`), 110-117 (`handleTracesLogs`), 119-149 (`handleTracesLive`), 154-176 (`authorizeTraceRequest`, `traceIDParam`) | Trace handlers + auth helpers to reuse for the new URL endpoint. |
| `internal/domain/telemetry.go` | 27-39 (`TraceInfo` struct) | Trace detail response DTO; add `URL` field here. |
| `internal/domain/config.go` | 25-30 (`ServerConfig`: `ControlAddr`, `DataAddr`, `DataHost`, `PublicURL`) | Add `PipelineURL` field. |
| `config/loader.go` | 24-27 (`server.*` defaults) | Add `v.SetDefault("server.pipeline_url", "")`. |
| `config/config.app.yaml` | 18 (`public_url`) | Add `pipeline_url` line. |
| `config/config.app.yaml.sample` | 22-27 (server block) | Add `pipeline_url` with comment. |
| `cmd/api/main.go` | 81-94 (run), 96-112 (cache scheme from `public_url`), 1084-1094 (`hostOf`/`validateCacheConfig`), 1138-1144 (`hostOf`) | Wire resolved pipeline base into `handler.ServerConfig`. |
| `internal/handler/test_helper_test.go` | 98-108 (`newTestEnv` builds `ServerConfig` without `PipelineURL`) | Test wiring; add `PipelineURL`. |
| `internal/service/connect_service.go` | 37, 56 | `ServerURL`/`DAGGER_CLOUD_URL` use `cfg.Server.PublicURL`. (No change required, but the wrapper will reuse the same config.) |
| `deploy/helm/dagger-kubernetes/values.yaml` | 96-100 (`supervisor.config.server`) | Add `pipelineUrl`. |
| `deploy/helm/dagger-kubernetes/templates/configmap.yaml` | 10-14 (`server:` block) | Add `pipeline_url` line. |
| `docs/README.md` | 316-345 (env + config tables) | Add `server.pipeline_url` row + new "Pipeline view URL" section. |
| `docs/design/` | ADR-001..020 exist | Add `ADR-021-pipeline-view-url.md`. |

No existing code derives a `dagger.cloud` URL server-side (grep for `dagger.cloud` in `internal/` is empty). The dagger.cloud string lives only in the upstream CLI.

## 4. Design decision (mechanism)

**Chosen (user-approved): Wrapper + platform URL endpoint.**

1. **`dagger-cache-ci` wrapper** is the supported "client" that displays the self-hosted URL. Fix it to:
   - Use the correct UI path `/pipelines/<traceID>`.
   - Derive the URL base from config (`server.pipeline_url` → `server.public_url` → `--server` flag) instead of requiring `--ui-url`.
   - Keep `--ui-url` / `--server` flags as overrides (backward compatible).

2. **New platform endpoint** `GET /api/v1/traces/:traceID/url` returns the self-hosted pipeline view URL for a trace. This is the platform "supplying" the self-hosted URL as a discoverable contract for any client (UI, CI integrations, a future CLI that consults it). Auth-gated by the existing `authorizeTraceRequest` (owner/member/admin; unknown meta → admin-only).

3. **Add `url` field** to the existing `GET /api/v1/traces/:traceID` (`TraceInfo`) response so the UI and clients fetching the full tree get the URL for free (same helper, near-zero cost).

4. **New optional config key** `server.pipeline_url` (empty = fall back to `server.public_url`). This deliberately does NOT reintroduce the dropped `server.ui_url` (the UI is still served from the control plane); `pipeline_url` is narrowly scoped to "the base URL used to build pipeline-view links", allowing a separate public UI host without coupling to the control-plane `public_url`.

5. **Centralize URL construction** in a pure domain helper (`internal/domain/pipeline_url.go`, stdlib only) used by both the handler and the wrapper — single source of truth for the path template.

**Explicitly out of scope:** forking/patching the upstream Dagger CLI; per-request URL overrides via headers/query; deriving the URL from `X-Forwarded-Host` (we use the configured public base, not the request Host, so link generation is stable behind proxies/TLS-terminating ingresses).

## 5. Exact data structures (Go)

### `internal/domain/config.go` — extend `ServerConfig`
```go
type ServerConfig struct {
    ControlAddr string `mapstructure:"control_addr"`
    DataAddr    string `mapstructure:"data_addr"`
    DataHost    string `mapstructure:"data_hostname"`
    PublicURL   string `mapstructure:"public_url"`
    PipelineURL string `mapstructure:"pipeline_url"` // NEW: base for pipeline-view links; "" => fall back to PublicURL
}
```

### `internal/domain/telemetry.go` — extend `TraceInfo`
```go
type TraceInfo struct {
    TraceID    string        `json:"trace_id"`
    RootSpan   *SpanNode     `json:"root_span"`
    Status     string        `json:"status"`
    StartTime  time.Time     `json:"start_time"`
    Duration   time.Duration `json:"duration_ns"`
    DurationMS int64         `json:"duration_ms"`
    Version    string        `json:"version"`
    CIProvider string        `json:"ci_provider,omitempty"`
    CIRepo     string        `json:"ci_repo,omitempty"`
    UserID     string        `json:"user_id,omitempty"`
    Username   string        `json:"username,omitempty"`
    URL        string        `json:"url,omitempty"` // NEW: self-hosted pipeline view URL
}
```

### `internal/domain/pipeline_url.go` — NEW (stdlib only)
```go
package domain

import (
    "fmt"
    "net/url"
    "regexp"
    "strings"
)

// pipelineViewPath is the UI route for a single trace (ui/src/router/index.ts).
const pipelineViewPath = "/pipelines/"

// traceIDRe bounds trace IDs reflected into URLs (mirrors handler.validTraceID).
var pipelineTraceIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// PipelineViewURL builds the self-hosted pipeline-view URL for a trace.
// base must be an absolute http(s) URL; its trailing slash is trimmed.
// traceID must be non-empty and match the safe charset.
// Returns a wrapped error on invalid input (never a partially-built URL).
func PipelineViewURL(base, traceID string) (string, error) {
    if base == "" {
        return "", fmt.Errorf("pipeline url base is empty: %w", ErrValidation)
    }
    u, err := url.Parse(base)
    if err != nil {
        return "", fmt.Errorf("parse pipeline url base: %w", err)
    }
    if u.Scheme != "http" && u.Scheme != "https" {
        return "", fmt.Errorf("pipeline url base must be http(s): %s: %w", base, ErrValidation)
    }
    if u.Host == "" {
        return "", fmt.Errorf("pipeline url base has no host: %s: %w", base, ErrValidation)
    }
    if traceID == "" || !pipelineTraceIDRe.MatchString(traceID) {
        return "", fmt.Errorf("invalid trace id: %q: %w", traceID, ErrValidation)
    }
    return fmt.Sprintf("%s://%s%s%s", u.Scheme, u.Host, pipelineViewPath, traceID), nil
}

// ResolvePipelineBase returns the effective pipeline-view base URL:
// pipelineURL when non-empty, else publicURL. Used by config-load validation
// and the CI wrapper.
func ResolvePipelineBase(publicURL, pipelineURL string) string {
    if pipelineURL != "" {
        return pipelineURL
    }
    return publicURL
}
```

> Note: `ErrValidation` already exists in `internal/domain` (used by `connect_service.go:65`). Confirm during implementation; if not present, define `var ErrValidation = errors.New("validation error")` in `internal/domain/errors.go` (stdlib only).

### `internal/handler/server.go` — extend `ServerConfig`
```go
type ServerConfig struct {
    ControlAddr  string
    DataAddr     string
    DataHost     string
    CacheHost    string
    CacheScheme  string
    CacheToken   string
    CollectorURL string
    VictoriaURL  string
    CertPath     string
    KeyPath      string
    PipelineURL  string // NEW: resolved base for pipeline-view links (absolute http(s) URL)
}
```

### New endpoint response shape (handler, inline struct)
```go
type traceURLResponse struct {
    TraceID string `json:"trace_id"`
    URL     string `json:"url"`
}
```

## 6. Exact function signatures (new/changed)

### `internal/domain/pipeline_url.go` (NEW — stdlib only)
- `func PipelineViewURL(base, traceID string) (string, error)`
- `func ResolvePipelineBase(publicURL, pipelineURL string) string`

### `config/loader.go`
- Add `v.SetDefault("server.pipeline_url", "")` near line 27.
- Add validation in a new `validateServerConfig(cfg *domain.Config) error` called from `Load` (after `validateAuthConfig`):
  ```go
  func validateServerConfig(cfg *domain.Config) error {
      base := domain.ResolvePipelineBase(cfg.Server.PublicURL, cfg.Server.PipelineURL)
      if base == "" {
          return fmt.Errorf("server.public_url must be set so a pipeline view URL can be derived")
      }
      if _, err := domain.PipelineViewURL(base, "traceid-placeholder"); err != nil {
          return fmt.Errorf("server.pipeline_url/public_url: %w", err)
      }
      return nil
  }
  ```
  (Uses a placeholder traceID to validate the base shape only.)

### `cmd/api/main.go`
- In `run(c *cli.Context)`, after `config.Load`, resolve and validate the pipeline base and pass it to `handler.ServerConfig`:
  ```go
  pipelineBase := domain.ResolvePipelineBase(cfg.Server.PublicURL, cfg.Server.PipelineURL)
  if _, err := domain.PipelineViewURL(pipelineBase, "traceid-placeholder"); err != nil {
      return fmt.Errorf("validate pipeline url: %w", err)
  }
  ```
  Wire `PipelineURL: pipelineBase` into the `handler.ServerConfig` literal constructed for `handler.NewServer`.

### `internal/handler/server.go`
- Route registration (in `configure()`), next to the other trace routes (~line 514):
  ```go
  h.GET("/api/v1/traces/:traceID/url", s.handleTracesURL)
  ```
- New handler:
  ```go
  func (s *Server) handleTracesURL(_ context.Context, c *app.RequestContext) {
      traceID, ok := s.authorizeTraceRequest(c)
      if !ok {
          return
      }
      u, err := domain.PipelineViewURL(s.cfg.PipelineURL, traceID)
      if err != nil {
          s.logger.WithError(err).WithField("trace_id", traceID).Warn("pipeline url misconfigured")
          writeError(c, consts.StatusInternalServerError, "pipeline url misconfigured")
          return
      }
      writeJSON(c, traceURLResponse{TraceID: traceID, URL: u})
  }
  ```
- In `handleTracesDetail` (`internal/handler/traces.go`), after enrichment, set `trace.URL`:
  ```go
  if u, err := domain.PipelineViewURL(s.cfg.PipelineURL, traceID); err == nil {
      trace.URL = u
  } else {
      s.logger.WithError(err).WithField("trace_id", traceID).Warn("pipeline url misconfigured")
  }
  ```
  (Best-effort: a misconfigured base does not break trace detail; logged.)

### `cmd/ci/main.go` (wrapper)
- Add a `--config` flag (default `config.app.yaml`) and load config via `config.Load` (gracefully ignore `ConfigFileNotFoundError` — `config.Load` already does).
- Resolve the URL base with precedence: `--ui-url` flag > `server.pipeline_url` (config) > `server.public_url` (config) > `--server` flag.
- Replace line 86 with the domain helper:
  ```go
  traceURL, err := domain.PipelineViewURL(uiURL, traceID)
  if err != nil {
      fmt.Fprintf(os.Stderr, "\nPipeline View: <unavailable: %v>\n", err)
  } else {
      fmt.Fprintf(os.Stderr, "\nPipeline View: %s\n", traceURL)
  }
  ```
- Keep `--server`/`--token` flags required (override config when set); when config provides `server.public_url`, `--server` may default to it.

## 7. Config: new keys, defaults, env vars, YAML, Helm

### `config/loader.go`
- `v.SetDefault("server.pipeline_url", "")`
- Env var: `DAGGER_CACHE_SERVER_PIPELINE_URL` (automatic via `v.AutomaticEnv()` + key replacer).

### `config/config.app.yaml` (line ~18)
```yaml
server:
  control_addr: ":8080"
  data_addr: ":8443"
  data_hostname: "data.supv.example.com"
  public_url: "https://supv.example.com"
  pipeline_url: ""  # Optional base for pipeline-view links (e.g. https://dagger-cache.example.com). Empty => use public_url.
```

### `config/config.app.yaml.sample` (server block, after `public_url` line 27)
```yaml
  pipeline_url: ""  # Optional base URL for pipeline-view links printed by the dagger-cache-ci wrapper and returned by GET /api/v1/traces/:id/url. Empty => fall back to public_url. Must be an absolute http(s) URL when set.
```
Also add to the env-var examples block near line 9:
```
#   server.pipeline_url       -> DAGGER_CACHE_SERVER_PIPELINE_URL
```

### `deploy/helm/dagger-kubernetes/values.yaml` (after `publicUrl` line 100)
```yaml
    ## @param supervisor.config.server.pipelineUrl Optional base URL for pipeline-view links (empty = use publicUrl). Must be absolute http(s) when set.
      pipelineUrl: ""
```

### `deploy/helm/dagger-kubernetes/templates/configmap.yaml` (after `public_url` line 14)
```yaml
      pipeline_url: {{ .Values.supervisor.config.server.pipelineUrl | quote }}
```

## 8. URL construction rules

- Path template: `<base-scheme>://<base-host>/pipelines/<traceID>` (matches `ui/src/router/index.ts` route `/pipelines/:id`).
- `base` = `server.pipeline_url` if non-empty, else `server.public_url`.
- Trailing slash on `base` is trimmed (the path constant `/pipelines/` carries the trailing slash).
- Only the scheme + host are taken from `base` (any path/query/fragment on the configured base is dropped) — keeps links stable and prevents path-injection via config.
- `traceID` validated against `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` (mirrors `handler.validTraceID`).
- Scheme must be `http` or `https`; host must be non-empty.
- IPv6 hosts: `url.Parse` handles `[::1]`; `u.Host` preserves brackets — emitted correctly.
- localhost / private IPs: allowed (dev mode).
- Proxies / TLS termination: the base is the **configured** public URL, never derived from request `Host`/`X-Forwarded-*`, so link generation is deterministic behind an ingress. (Per-request override is explicitly out of scope.)

## 9. Edge cases

| Case | Behavior |
|---|---|
| `server.pipeline_url` empty | Fall back to `server.public_url` (backward compatible). |
| `server.public_url` also empty | `config.Load` fails with a clear validation error (server.public_url must be set). |
| `pipeline_url` missing scheme | `config.Load` fails: `server.pipeline_url/public_url: pipeline url base must be http(s)`. |
| `pipeline_url` has no host | `config.Load` fails: `pipeline url base has no host`. |
| `pipeline_url` has a path/query | Path/query silently dropped (only scheme+host used). |
| `pipeline_url` trailing slash | Trimmed. |
| Invalid `traceID` at the endpoint | `authorizeTraceRequest` → `traceIDParam` returns 400 "missing trace ID" for empty; for non-empty but unsafe charset, `PipelineViewURL` returns an error → 500 "pipeline url misconfigured" (the endpoint never builds a partial URL). Note: `traceIDParam` currently only rejects empty; unsafe charset is caught by `PipelineViewURL`. |
| Unknown traceID (no trace_meta, no Tempo) | `handleTracesURL` reuses `authorizeTraceRequest` → `authorizeTrace` which is admin-only for unknown meta. Non-admins get 403/404 per existing behavior. The URL is still returned for admins (URL derivation does not require the trace to exist in Tempo). |
| Misconfigured base at runtime (shouldn't happen — validated at load) | Handler logs a WARN and returns 500; trace detail omits `url`. |
| Wrapper: `dagger` prints no traceID | `extractTraceID` returns "" → no `Pipeline View:` line printed (existing behavior preserved). |
| Wrapper: config file missing | `config.Load` skips (ConfigFileNotFoundError) → `--server`/`--token` flags required as today. |

## 10. Error handling & logging

- All errors wrapped with `%w` (e.g. `fmt.Errorf("validate pipeline url: %w", err)`).
- Handler errors via existing `writeError(c, status, msg)` helper.
- Structured logging via `s.logger.WithError(err).WithField("trace_id", traceID).Warn("pipeline url misconfigured")`.
- Wrapper prints `<unavailable: ...>` to stderr on URL build failure (does not exit non-zero for a URL error; the `dagger` exit code is preserved as today).
- No string `+` concatenation — all via `fmt.Sprintf`.

## 11. Validation strategy

- **Config-load level** (`config.Load`): `validateServerConfig` ensures the resolved base is an absolute `http(s)` URL with a host (using `PipelineViewURL` with a placeholder traceID). Fails fast at startup.
- **Domain level** (`PipelineViewURL`): validates base scheme/host and traceID charset on every call (defense-in-depth; the endpoint and wrapper both rely on it).
- **No** per-request URL override (global config only).

## 12. Test plan (standard `testing`, table-driven, 100% package coverage target)

### `internal/domain/pipeline_url_test.go` (NEW)
- `TestPipelineViewURL` (table-driven): valid `https://supv.example.com` + `abc123` → `https://supv.example.com/pipelines/abc123`; trailing slash trimmed; `http://localhost:8080`; IPv6 `https://[::1]`; path/query on base dropped; empty base → error; missing scheme → error; `ftp://` scheme → error; no host → error; empty traceID → error; unsafe traceID charset → error.
- `TestResolvePipelineBase`: pipelineURL wins when non-empty; falls back to publicURL when empty; both empty → "".

### `config/loader_test.go` (extend)
- `TestLoadServerPipelineURLDefault`: default `server.pipeline_url` is "".
- `TestLoadServerPipelineURLEnvOverride`: `DAGGER_CACHE_SERVER_PIPELINE_URL=https://x.example.com` overrides.
- `TestLoadInvalidPipelineURL`: `server.pipeline_url: "ftp://x"` → load error.
- `TestLoadEmptyPublicURL`: both `public_url` and `pipeline_url` empty → load error.
- `TestLoadPipelineURLFallbackValid`: `pipeline_url` empty, `public_url` valid → loads OK.

### `internal/handler/traces_test.go` (extend) + new `internal/handler/pipeline_url_test.go`
- `TestHandleTracesURL`: admin GETs `/api/v1/traces/<id>/url` → 200 `{trace_id, url}` with `url == <base>/pipelines/<id>`; non-owner non-admin → 403/404 (per `authorizeTrace`); missing traceID → 400; misconfigured base (inject empty `PipelineURL` via a test-only server) → 500.
- `TestHandleTracesDetailIncludesURL`: extend the existing detail test to assert `url` field present and correct; add a case where base is empty → `url` omitted and status still 200.

### `internal/handler/test_helper_test.go` (extend)
- Add `PipelineURL: "https://supv.example.com"` to the `ServerConfig` in `newTestEnv` (and the integration helper) so handler tests have a stable base.

### `cmd/ci/main_test.go` (NEW)
- `TestExtractTraceID`: table-driven (32-hex present, multiple matches, none).
- `TestResolveUIBase`: precedence `--ui-url` > config `pipeline_url` > config `public_url` > `--server`.
- `TestRunPrintsPipelineView`: run wrapper with a stubbed `dagger` (fake on PATH) that prints a traceID to stderr; assert stderr contains `Pipeline View: https://supv.example.com/pipelines/<id>` (correct path).
- `TestRunNoTraceID`: stub `dagger` prints no traceID → no `Pipeline View:` line.

### `tests/integration/` (extend `api_test.go` or new `pipeline_url_test.go`)
- `TestPipelineViewURLEndpoint`: provision an engine (`POST /v1/engines` with a `trace_id`), then `GET /api/v1/traces/<trace_id>/url` with the admin bearer → 200, `url == <publicURL>/pipelines/<trace_id>`. Proves the contract against the real handler wiring.
- `TestCIWrapperPrintsSelfHostedURL` (integration): build a tiny fake `dagger` binary on PATH that emits a known traceID to stderr; invoke `cmd/ci/main.go`'s `run` (or the compiled `dagger-cache-ci` with `--server`/`--token` pointing at the integration server) and assert the printed `Pipeline View:` line uses the configured base + `/pipelines/<id>`. Proves the client-facing behavior end-to-end.

## 13. Documentation updates (exact files)

1. `config/config.app.yaml` — add `pipeline_url: ""` line + comment.
2. `config/config.app.yaml.sample` — add `pipeline_url` in server block + env-var example line.
3. `docs/README.md`:
   - Env-var table (~line 318): add `server.pipeline_url` → `DAGGER_CACHE_SERVER_PIPELINE_URL`.
   - Config reference table (~line 344): add `pipeline_url` row.
   - New subsection under the existing trace/pipeline docs: "Pipeline view URL" — explain the stock-CLI `URLForTrace` limitation, that the `dagger-cache-ci` wrapper prints the self-hosted link, the `GET /api/v1/traces/:id/url` endpoint contract, and how to set `server.pipeline_url`.
4. `docs/design/ADR-021-pipeline-view-url.md` (NEW) — record the decision: stock CLI hardcodes dagger.cloud; we supply the self-hosted URL via the wrapper + a dedicated endpoint + optional `server.pipeline_url` config; path template `/pipelines/<id>`; out-of-scope items.
5. `deploy/helm/dagger-kubernetes/values.yaml` — add `supervisor.config.server.pipelineUrl: ""` with `@param` comment.
6. `deploy/helm/dagger-kubernetes/templates/configmap.yaml` — add `pipeline_url:` line.
7. `cmd/ci/main.go` — update `--ui-url` flag usage text to mention config precedence and the `/pipelines/` path.

## 14. Implementation order (files in order)

1. `internal/domain/pipeline_url.go` (+ `pipeline_url_test.go`) — pure helper, stdlib only.
2. `internal/domain/config.go` — add `PipelineURL` field to `ServerConfig`.
3. `internal/domain/telemetry.go` — add `URL` field to `TraceInfo`.
4. `config/loader.go` — add `SetDefault` + `validateServerConfig`; extend `loader_test.go`.
5. `internal/handler/server.go` — add `PipelineURL` to `ServerConfig`; register `GET /api/v1/traces/:traceID/url`; add `traceURLResponse` + `handleTracesURL`.
6. `internal/handler/traces.go` — set `trace.URL` in `handleTracesDetail`.
7. `internal/handler/test_helper_test.go` — add `PipelineURL` to test envs.
8. `internal/handler/traces_test.go` / new `pipeline_url_test.go` — endpoint + detail URL tests.
9. `cmd/api/main.go` — resolve + validate pipeline base; wire `PipelineURL` into `handler.ServerConfig`.
10. `cmd/ci/main.go` — load config, resolve base with precedence, use `domain.PipelineViewURL`, fix path; add `cmd/ci/main_test.go`.
11. `config/config.app.yaml` + `config/config.app.yaml.sample` — add `pipeline_url`.
12. `deploy/helm/dagger-kubernetes/values.yaml` + `templates/configmap.yaml` — add `pipelineUrl` / `pipeline_url`.
13. `docs/README.md` + `docs/design/ADR-021-pipeline-view-url.md` — docs.
14. `tests/integration/` — add `TestPipelineViewURLEndpoint` + `TestCIWrapperPrintsSelfHostedURL`.

### Verification commands (run after each Go change and at the end)
```bash
gofmt -l .                         # no output
goimports -l .                      # no output (local prefix github.com/disaster/dagger-kubernetes)
go vet ./...
go build ./...
go test ./...
```
Then per `AGENTS.local.md` §6: rebuild image, push, `helm upgrade` (re-capture values, set `supervisor.config.raft.replicas=1`), rollout restart, run §5.1 agent verification, request §5.2 human verification of the live UI (and a wrapper run printing the correct `https://dagger.home.webcenter.fr/pipelines/<id>` link).

## 15. Risks & uncertainties

1. **Stock CLI cannot be redirected (hard constraint).** Verified live on `main`: `URLForTrace` hardcodes `https://dagger.cloud/`. The platform has no server-side hook to change the stock CLI's printed URL. This plan delivers the self-hosted URL via the wrapper + a discoverable endpoint; users running the bare `dagger` CLI (without the wrapper) will still see `dagger.cloud`. Documented in ADR-021 and `docs/README.md`.
2. **Wrapper traceID extraction is fragile.** `extractTraceID` (`cmd/ci/main.go:102`) regex-scans stderr for `[a-f0-9]{32,}`. If the Dagger CLI changes its output format, extraction may miss or mis-capture. Mitigation: the new endpoint lets CI integrations resolve the URL from a known traceID without parsing CLI output; the wrapper remains a best-effort convenience. Track upstream `engine/telemetry/url.go` + CLI output format across releases.
3. **Path fix is a behavior change.** The wrapper previously printed `/traces/<id>` (broken). Switching to `/pipelines/<id>` is a bug fix but changes the printed string; any consumer that string-matched the old path will need updating. Low risk (the old path 404'd in the UI).
4. **`ErrValidation` existence.** The plan assumes `domain.ErrValidation` exists (used by `connect_service.go`). Implementer must confirm; if absent, define it in `internal/domain` (stdlib only).
5. **`traceIDParam` charset gap.** `traceIDParam` only rejects empty; unsafe-charset traceIDs reach `PipelineViewURL`, which rejects them → 500. Acceptable (URL never built), but could be tightened to 400 by adding a charset check in `traceIDParam` — left as a minor follow-up to avoid changing shared behavior.
6. **No per-request override.** Operators behind a different public host than `server.public_url` must set `server.pipeline_url`; there is no header/query override. By design (deterministic link generation).

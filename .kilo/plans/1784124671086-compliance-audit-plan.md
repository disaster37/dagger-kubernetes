# Compliance Audit & Remediation Plan — dagger-cache

- **Status:** implementation-ready (audit complete; remediation NOT yet applied — see constraint below)
- **Module:** `github.com/disaster/dagger-kubernetes`
- **Audited against:** `AGENTS.md` + `CONTRIBUTING.md` (do NOT modify either without validation — out of scope per user rule)
- **Goal:** Bring every file in the repo into full compliance with the mandatory rules.

> **ENVIRONMENT CONSTRAINT (important):** The orchestrator that produced/validated this plan ran in a **planning-only environment**: it has **no shell/exec tool** (bash deny `*`) and **cannot edit source or doc files** — only `.kilo/plans/*.md` is writable (`edit`/`write` deny `*` for non-plan paths). Therefore **none of the remediation tasks below have been applied yet.** All tasks are pending. The plan is the hand-off artifact for a **shell-capable + edit-capable** session (e.g. an Agent Manager worktree session with full permissions) to execute in the listed phase order, verifying with `go build`/`go test`/`golangci-lint` after each phase.

---

## Summary

The project is **substantially non-compliant** with its own `AGENTS.md`/`CONTRIBUTING.md` rules. The three mandatory library choices are violated at the root: `go.mod` declares `go.uber.org/zap`, `github.com/gorilla/websocket`, and `github.com/pkg/errors`, while **omitting** the required `github.com/urfave/cli/v2`, `github.com/sirupsen/logrus`, and `github.com/cloudwego/hertz`. As a consequence: logging is `zap` everywhere (8 files), the entire HTTP control plane is `net/http` instead of Hertz (`internal/api/*`), the live-trace stream uses `gorilla/websocket` instead of Hertz native SSE, and both CLI entry points use the stdlib `flag` package instead of `urfave/cli/v2`.

Beyond the stack mismatch, the audit found: Viper config is missing `SetDefault()` for 10 fields that appear in the structs/sample, plus two sample-vs-default value mismatches (`tokens_file`, `fleet.namespace`); documentation drift in `docs/README.md`, `config.app.yaml`, the Helm `configmap.yaml`/`values.yaml` (phantom `ui.*`, `ui_url`, `prometheus_url`, `DAGGER_CACHE_PROVIDER`, nonexistent source path `internal/dataplane`); a missing `docs/design/` ADR directory; test coverage far below the 100% target (6 internal packages have **no** `_test.go`); tests using `zap.NewNop()` instead of `logrus.New()+io.Discard`; bare `return nil, err` without `%w` wrapping in several places; an auth layer that only parses `Basic` (contradicting the documented "bearer tokens" contract); package-level `promauto` globals violating the no-global-state rule; and minor error-context/leak issues. Positively: **no `+` string concatenation** anywhere, **no `init()` wiring**, **no `fmt.Println`/`log.Print`** in production paths, config structs all carry `mapstructure` tags, import order is stdlib→third-party→project, and `.golangci.yml` already enforces `goimports` with the correct local prefix.

The remediation is ordered so dependencies resolve cleanly: fix `go.mod` → migrate logger to logrus → migrate HTTP server to Hertz → migrate SSE to Hertz `pkg/protocol/sse` → migrate CLIs to `urfave/cli/v2` → fix Viper defaults → fix error wrapping/auth/metrics DI → add tests → fix docs/ADRs.

---

## Findings

> Severity legend: **Critical** = violates a mandatory library/rule and blocks the build contract · **High** = violates a mandatory rule, affects many files or correctness · **Medium** = localized rule violation · **Low** = nuance/edge-case.

### F1 — Critical — `go.mod` declares forbidden deps and omits required deps
- **Files:** `go.mod:5-11`, `go.sum`.
- **Rule violated:** "Required libraries (do not deviate)"; rules 3 (logrus), 4 (hertz), 5 (hertz SSE), 7 (urfave/cli).
- **Current:** `go.mod` requires `go.uber.org/zap v1.28.0`, `github.com/gorilla/websocket v1.5.3`, `github.com/pkg/errors v0.9.1` (indirect); does NOT require `urfave/cli/v2`, `logrus`, or `hertz`.
- **Fix:** Remove zap, gorilla/websocket, pkg/errors; add `github.com/urfave/cli/v2`, `github.com/sirupsen/logrus`, `github.com/cloudwego/hertz`, `github.com/hertz-contrib/reverseproxy`, `github.com/hertz-contrib/adapter`. Keep `github.com/disaster37/goca`, `prometheus/client_golang`, `spf13/viper`. Run `go mod tidy`.

### F2 — Critical — HTTP control plane uses `net/http`, not Hertz
- **Files:** `internal/api/server.go`, `ui.go`, `metrics.go`, `logs.go`, `traces.go`.
- **Rule violated:** 4 (Hertz only) + 11 (middleware) + 15 (structure).
- **Current:** `mux := http.NewServeMux()`, `http.Server{Handler}`, `http.FileServer`, `httputil.NewSingleHostReverseProxy`, handlers `func(w http.ResponseWriter, r *http.Request)`.
- **Fix:** `server.Default(server.WithHostPorts(cfg.ControlAddr))`; routes via `h.GET/POST/Any` incl. `/healthz`, `/readyz`, `h.Any("/metrics", adapter.HTTPHandler(promhttp.Handler()))`; handlers → `func(ctx context.Context, c *app.RequestContext)`; `writeError`/`writeJSON` → `c.SetStatusCode`+`c.JSON` (`{"message":"..."}`); reverse proxies → `hertz-contrib/reverseproxy`; middleware via `h.Use(...)`. The TLS L4 data plane (`handleDataConn` raw `tls.Listener`/`io.Copy`) legitimately stays on `net`/`crypto/tls`.

### F3 — Critical — Logging uses `go.uber.org/zap`, not `logrus`
- **Files:** `internal/observ/logger.go`, `internal/api/server.go`, `ui.go`, `metrics.go`, `traces.go`, `internal/auth/token.go`, `internal/fleet/manager.go`, `cmd/supervisor/main.go`.
- **Rule violated:** 3 (logrus, JSON formatter `TimestampFormat: 2006-01-02T15:04:05.000Z07:00`).
- **Fix:** `observ.NewLogger` returns `*logrus.Logger` with `JSONFormatter{TimestampFormat:"2006-01-02T15:04:05.000Z07:00"}`, `ParseLevel` fallback `InfoLevel`. Every `*zap.Logger` field/param → `*logrus.Logger`; `zap.X` → `logger.WithFields(logrus.Fields{...})`/`WithError`. Remove `defer logger.Sync()`. Add `observ.NewTestLogger()` → `logrus.New()`+`SetOutput(io.Discard)`.

### F4 — High — CLI entry points use stdlib `flag`, not `urfave/cli/v2`
- **Files:** `cmd/supervisor/main.go`, `cmd/dagger-cache-ci/main.go`.
- **Rule violated:** 7 (urfave/cli App shape).
- **Fix:** Both → `&cli.App{Name:..., Usage:..., Flags:[]cli.Flag{&cli.StringFlag{Name:"config", Value:"config.app.yaml", ...}}, Action: run}`; `app.Run(os.Args)`. CI flags: `server`/`token` (required), `ui-url`, `cache-registry` (default `cache.reg/dagger-cache`), `version`, `ci`; pass-through via `c.Args().Slice()`. Keep `log.Fatal(err)` only in `main`.

### F5 — High — Live trace streaming uses `gorilla/websocket`, not Hertz native SSE
- **Files:** `internal/api/traces.go`, `internal/telemetry/live.go`.
- **Rule violated:** 5 (Hertz `pkg/protocol/sse` for server→client push).
- **Fix:** `import "github.com/cloudwego/hertz/pkg/protocol/sse"`. `handleTracesLive`: set `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`; `w := sse.NewWriter(c); defer w.Close()`. `LiveClient.Conn` → SSE writer abstraction. `writePump` → `WriteEvent("message", string(data))` + heartbeat comments. Remove gorilla import.

### F6 — High — Viper config missing `SetDefault()` for 10 fields + sample/value mismatches
- **File:** `internal/config/config.go`; `config.app.yaml.sample`.
- **Rule violated:** 8 (every field needs `v.SetDefault()`) + 12 (sample in sync with `config.go`).
- **Missing defaults:** `cache.public_host`, `cache.internal_addr`, `cache.s3.bucket`, `cache.s3.region`, `auth.oauth.client_id`, `auth.oauth.client_secret`, `auth.oauth.redirect_url`, `auth.oauth.allowed_orgs`, `version.allowlist`, `otel.otlp_endpoint`.
- **Mismatches:** `config.go` default `auth.internal.tokens_file=/etc/dagger-cache/tokens` vs sample `/etc/dagger-kubernetes/tokens`; `config.go` default `fleet.namespace=dagger-cache` vs sample `dagger-kubernetes`.
- **Fix:** Add the 10 `v.SetDefault(...)` calls (empty strings/`[]string{}` for secrets/optional lists; `otel.otlp_endpoint` default `""`). Set sample `tokens_file`→`/etc/dagger-cache/tokens` and `fleet.namespace`→`dagger-cache`.

### F7 — High — Documentation drift in `docs/README.md`, `config.app.yaml`, Helm chart
- **Files:** `docs/README.md`, `config.app.yaml`, `helm/dagger-kubernetes/values.yaml`, `helm/dagger-kubernetes/templates/configmap.yaml`.
- **Rule violated:** 12 (docs current; sample/config in sync).
- **Current:** README references `ui.enabled`, `ui.spa_dir`, `server.ui_url`, `DAGGER_CACHE_PROVIDER`, `prometheus_url` (none exist in `config.go`); README links `internal/dataplane` (no such package; the L4 proxy lives in `internal/api/server.go` `handleDataConn`). `config.app.yaml`+configmap emit `server.ui_url`; configmap emits `tokens_file: /etc/dagger-kubernetes/tokens`; values+configmap emit `prometheus_url`/`prometheusUrl`; configmap emits a `ui:` block. Note: `DAGGER_CACHE_PROVIDER` IS set in `deploy/docker/docker-compose.yaml` but is NOT wired into `cmd/supervisor/main.go` (hardcodes `fleet.NewStubProvider()`), so README's "selected by DAGGER_CACHE_PROVIDER" is inaccurate.
- **Fix:** Remove `server.ui_url` from `config.app.yaml`+Helm `values.yaml`/`configmap.yaml`; rename `prometheus_url`/`prometheusUrl`→`victoria_url`/`victoriaUrl` in README+Helm; remove the `ui:` block from Helm configmap; reframe README's `DAGGER_CACHE_PROVIDER` claim (stub provider today); fix README `internal/dataplane`→`internal/api` (L4 proxy); reconcile `--config` default wording (`config.app.yaml` is the flag default).

### F8 — High — `docs/design/` ADR directory is missing
- **Files:** `docs/` has only `README.md`.
- **Rule violated:** 12 + 15.
- **Fix:** Create `docs/design/` with `index.md` + `ADR-001-mandatory-stack.md`, `ADR-002-net-http-to-hertz-migration.md`, `ADR-003-sse-via-hertz-native.md`, `ADR-004-per-version-statefulset-autoscaler.md`, `ADR-005-embedded-minting-ca.md`, `ADR-006-oci-registry-cache-backend.md`, `ADR-007-outbound-http-clients.md`, `ADR-008-prometheus-metrics-di.md`.

### F9 — High — Test coverage far below the 100% target
- **Files with NO `_test.go`:** `internal/api/*`, `internal/auth/token.go`, `internal/observ/*`, `internal/cache/cache.go`, `internal/telemetry/*`, `internal/config/config.go`; `internal/ca/providers.go` untested.
- **Rule violated:** 9.
- **Fix:** Add `*_test.go` (table-driven, happy+error paths, stubs, `logrus.New()+io.Discard`). Standard `testing` only. Do this AFTER F3 (tests must use logrus, not zap).

### F10 — High — Tests use `zap.NewNop()` instead of `logrus.New()+io.Discard`
- **Files:** `internal/fleet/manager_test.go`, `test/integration_test.go`.
- **Fix:** Replace with `observ.NewTestLogger()` (from F3). Must be done with F3.

### F11 — Medium — Bare `return nil, err` / `return err` without `%w` wrapping
- **Files:** `internal/fleet/manager.go` (`GetVersionFleet` L134, `sweepVersion` L171, `ScaleToZero` L218/L228, `AllFleetInfo` L237); `internal/config/config.go` (L174, L180); `internal/version/version.go` (`ResolveMinimal` L90).
- **Rule violated:** 2 (wrap with `%w` via `fmt.Errorf`).
- **Fix:** Wrap each, e.g. `return nil, fmt.Errorf("get replicas: %w", err)`, `return nil, fmt.Errorf("read config: %w", err)`, `return nil, fmt.Errorf("unmarshal config: %w", err)`, `return nil, fmt.Errorf("parse version: %w", err)`.
- **⚠️ Import note:** `internal/config/config.go` currently imports only `time` + `viper` — it does NOT import `fmt`. When applying the `%w` wraps there, **add `"fmt"` to the import block** or it will not compile. (`fleet/manager.go` and `version/version.go` already import `fmt`.)

### F12 — Medium — Auth supports only `Basic`, not `Bearer` (contradicts "bearer tokens" contract)
- **File:** `internal/auth/token.go` (`extractToken`).
- **Rule violated:** 12 (doc/contract) + Dagger-Cloud-compatible contract.
- **Current:** Only `Basic ` prefix handled; `Bearer ` rejected.
- **Fix:** Handle `Bearer <token>` first (return trimmed token), keep `Basic` fallback. Add table-driven tests for Bearer, Basic, missing, unsupported.

### F13 — Medium — `promauto` package-level globals violate "no global state"
- **File:** `internal/observ/metrics.go`.
- **Rule violated:** 10 (DI via constructors; no global state; no `init()`).
- **Fix:** Refactor to `type Metrics struct{...}` + `func NewMetrics(reg prometheus.Registerer) *Metrics`, injected into `Server`/`Manager`. Update all call sites (`observ.EngineAcquireTotal...` → `s.metrics.EngineAcquireTotal...`). Coupled to F2/F3 migration.

### F14 — Medium — `ca.NewMintingCAFromPEM` PEM-block error messages are vague
- **File:** `internal/ca/ca.go:73-87`.
- **⚠️ Correction to original finding:** `encoding/pem.Decode` signature is `func Decode(data []byte) (p *Block, rest []byte)` — it returns **no error**. The `_` in `certBlock, _ := pem.Decode(certPEM)` is the `rest` slice, NOT a discarded error. There is therefore **no error to capture or `%w`-wrap**. The original "discards the pem.Decode error" finding was based on a misreading of the stdlib API.
- **Fix (minor, optional):** Improve the `nil`-block messages for clarity, e.g. `fmt.Errorf("decode CA cert PEM: no PEM block found")` and `fmt.Errorf("decode CA key PEM: no PEM block found")`. No `%w` (nothing to wrap). This is low-value; safe to defer.

### F15 — Low — Telemetry outbound HTTP clients use `net/http`
- **Files:** `internal/telemetry/traces.go`, `logs.go`, `metrics.go`.
- **Fix (optional):** Keep `net/http` for outbound backend queries (stdlib, lowest risk); document the exception in `ADR-007-outbound-http-clients.md`.

### F16 — Low — Internal error strings leaked to clients via `writeError(..., err.Error())`
- **File:** `internal/api/server.go:239,256,350`.
- **Fix:** Log the detailed `err` server-side and return a static message: L239 `"invalid image"`, L256 `"no engine capacity"`, L350 `"fleet unavailable"`. (Use the current logger style — zap until F3, logrus after.)

### F17 — Low — Dockerfile builds only `supervisor`; README references building `dagger-cache-ci`
- **Files:** `Dockerfile`, `docs/README.md:277`.
- **Fix (optional):** Add a `dagger-cache-ci` build stage or clarify README.

---

## Remediation Tasks (ALL PENDING — none applied; environment blocked edits)

Execute in order; verify after each phase. `fmt.Sprintf` for strings, `%w` for errors, logrus for logging, Hertz for HTTP/SSE, urfave/cli for CLIs.

### Phase 1 — Dependencies (F1) — needs `go mod tidy`
- **T1.** Edit `go.mod`: remove zap, gorilla/websocket, pkg/errors; add `urfave/cli/v2`, `logrus`, `hertz`, `hertz-contrib/reverseproxy`, `hertz-contrib/adapter`. Run `go mod tidy`. **Validate:** `go build ./...`; forbidden deps absent from `go.mod`.

### Phase 2 — Logger migration (F3, F10 helper)
- **T2.** Rewrite `internal/observ/logger.go`: `func NewLogger(level string) (*logrus.Logger, error)` + `func NewTestLogger() *logrus.Logger`.

### Phase 3 — HTTP server migration to Hertz (F2, F16)
- **T4.** Rewrite `internal/api/server.go` to Hertz handlers/middleware/reverse-proxy/`adapter.HTTPHandler(promhttp.Handler())` for `/metrics`. Apply F16 static client messages.
- **T5.** Rewrite `internal/api/ui.go` to serve embedded `ui-dist` via Hertz.
- **T6.** Rewrite `internal/api/metrics.go`, `logs.go`, `traces.go` (non-live) to `app.RequestContext` + logrus.
- **T7.** Update `internal/auth/token.go`: `ValidateRequest` to read headers from `*app.RequestContext`; logger → `*logrus.Logger`. (F12 Bearer in Phase 7.)

### Phase 4 — SSE migration (F5)
- **T8.** Rewrite `internal/telemetry/live.go`: `LiveClient.Conn` → SSE writer abstraction.
- **T9.** Rewrite `internal/api/traces.go handleTracesLive`: Hertz `sse.NewWriter(c)`; set SSE headers.

### Phase 5 — CLI migration (F4)
- **T10.** Rewrite `cmd/supervisor/main.go` to urfave/cli/v2; logrus logger.
- **T11.** Rewrite `cmd/dagger-cache-ci/main.go` to urfave/cli/v2.

### Phase 6 — Viper config defaults + sample sync (F6) — safe, no new deps
- **T12.** `internal/config/config.go Load()`: add the 10 missing `v.SetDefault(...)` calls; wrap the two bare returns (F11) — **and add `"fmt"` to the import block**.
- **T13.** `config.app.yaml.sample`: set `auth.internal.tokens_file`→`/etc/dagger-cache/tokens` (L27) and `fleet.namespace`→`dagger-cache` (L59). 2-space YAML.

### Phase 7 — Error wrapping + auth Bearer + metrics DI (F11, F12, F14, F13)
- **T14.** `internal/fleet/manager.go`: wrap bare returns at L134/L171/L218/L228/L237 with `%w`.
- **T15.** `internal/version/version.go:90`: `return nil, fmt.Errorf("parse version: %w", err)`.
- **T16.** `internal/ca/ca.go:74,84`: (optional) clarify PEM-block messages (no `%w`; pem.Decode returns no error).
- **T17.** `internal/auth/token.go extractToken`: add `Bearer` handling before `Basic` (F12) + table tests.
- **T18.** `internal/observ/metrics.go`: refactor to injected `Metrics` struct (F13). Coupled to F2/F3.

### Phase 8 — Tests (F9, F10) — after F3
- **T19–T26.** Add `*_test.go` for `internal/observ`, `config`, `cache`, `auth`, `telemetry`, `ca/providers`, `api`; update `fleet/manager_test.go` + `test/integration_test.go` to `observ.NewTestLogger()` and Bearer auth.

### Phase 9 — Documentation + ADRs (F7, F8, F15, F17) — safe, no deps
- **T27.** Fix `docs/README.md`: L236 remove `/ ui_url`; L243 `prometheus_url`→`victoria_url`; L263-264 remove `ui` config rows; L309-312 reframe `DAGGER_CACHE_PROVIDER` (stub today); L382 Prometheus→VictoriaMetrics/`victoria_url`; L386-388 reframe UI serving (embedded `ui-dist`, always on, no `ui.spa_dir`/`ui.enabled`); L499-509 `internal/dataplane`→`internal/api` (L4 proxy) and remove `internal/cloud` literal; L194-196 reconcile `--config` default (`config.app.yaml`).
- **T28.** `config.app.yaml:19` remove `server.ui_url`; Helm `values.yaml` remove `uiUrl` (L27), rename `prometheusUrl`→`victoriaUrl` (L41, value `http://victoria:8428`); Helm `configmap.yaml` remove `ui_url` line (L15), `tokens_file`→`/etc/dagger-cache/tokens` (L19), `prometheus_url`→`victoria_url` (L34), remove `ui:` block (L61-63).
- **T29.** Create `docs/design/` ADRs (ADR-001..008 per F8).
- **T30.** (Optional, F17) Dockerfile `dagger-cache-ci` stage.

---

## Deferred (requires shell-capable + edit-capable session)

ALL phases are pending because the producing environment could neither edit source/docs nor run go tooling. Recommended: open an Agent Manager worktree session (or a session with full edit+shell permissions) and run Phase 1→2→3→4→5→6→7(F13)→8→9 in order, verifying with `go build ./...`, `go test -race -coverprofile=coverage.out -covermode=atomic ./...`, and `golangci-lint run ./...` after each phase. Phases 6 and 9 are safe/decoupled and can be done first if desired.

---

## Validation

Run from `/projects/dagger-cache` after full remediation:

```bash
go build ./...
gofmt -l .
goimports -l -local github.com/disaster/dagger-kubernetes .
go test -race -coverprofile=coverage.out -covermode=atomic ./...
go tool cover -func=coverage.out | grep total
golangci-lint run ./...
grep -E "go.uber.org/zap|gorilla/websocket|pkg/errors" go.mod          # empty
grep -E "urfave/cli/v2|sirupsen/logrus|cloudwego/hertz" go.mod         # all three
grep -Rn "zap\." internal/ cmd/ test/                                  # empty
grep -Rn "flag.String\|flag.Parse" cmd/                                # empty
grep -Rn "http.NewServeMux\|http.HandlerFunc\|httputil.NewSingleHostReverseProxy" internal/api/  # empty
grep -Rn "websocket" internal/api/ internal/telemetry/                # empty
grep -n "return nil, err$\|return err$" internal/                     # empty
grep -Rn "ui_url\|prometheus_url\|DAGGER_CACHE_PROVIDER\|internal/dataplane\|internal/cloud" docs/ config.app.yaml helm/  # empty
ls docs/design/*.md                                                    # ADRs present
```

---

## Risks & Edge Cases

- **Planning-only environment:** edits to source/docs and shell were denied in the producing session; nothing applied. Re-run in a capable session.
- **Hertz reverse-proxy parity:** OTel/metrics/cache proxies use custom `Director`/path rewrites; port to `hertz-contrib/reverseproxy` options and verify via contract tests.
- **`promhttp` on Hertz:** mount via `hertz-contrib/adapter.HTTPHandler`; confirm `/metrics` exposes all five collectors.
- **TLS L4 data plane:** `handleDataConn` is raw `tls.Listener`/`io.Copy` — not HTTP; stays on `net`/`crypto/tls`.
- **AGENTS.md internal contradiction:** "Middleware pattern (net/http)" section shows `http.Handler` middleware while the HTTP rule mandates Hertz. Cannot fix without editing AGENTS.md (out of scope). Implement Hertz middleware regardless; note in an ADR.
- **SSE writer lifetime / unsubscribe race:** ensure the Hertz response writer is closed and `writePump` exits on `ctx.Done()`.
- **Auth contract change:** adding `Bearer` must keep `Basic` fallback; update integration test to Bearer.
- **Metrics DI churn:** injected `Metrics` struct touches `api/server.go` + `fleet/manager.go`; use a default registry in tests to avoid double-registration panics.
- **Helm values rename:** `prometheusUrl`→`victoriaUrl`, remove `uiUrl` is breaking for existing `my-values.yaml` users; document in an ADR/CHANGELOG.
- **F14 correction:** `pem.Decode` returns no error; do not fabricate a `%w` wrap there.
- **F11 import note:** `config.go` needs `"fmt"` added when the `%w` wraps are applied.

---

## Reviewer Validation & Additional Findings (Step 4 — read-only review)

An independent read-only code review **CONFIRMED all 17 findings (F1–F17)**; none refuted, none overstated. It also validated: the **F14 `pem.Decode` correction is correct** (no error return; the `_` discards the `rest` slice), the **F11 `fmt`-import note for `config.go` is correct** (it imports only `time`+`viper`), and the **phase ordering is sound** (Phase 1 first; Phase 2 before 3; Phase 3 before 4; Phases 6 & 9 are decoupled and may run first).

Additional issues found by the reviewer — **fold into the relevant remediation phases**:

- **B1 (HIGH, correctness)** — OTel proxy double-counts the `success` metric: `internal/api/server.go:334` always increments `success` after `proxy.ServeHTTP`, even when the `ErrorHandler` (L328) already incremented `error`. One failed request counts as both. Fix during **Phase 3 (T4)**: gate the `success` incr behind a `proxyOK` bool set `false` in the error handler. *(Corroborated by the Step-3 simplifier.)*
- **B2 (HIGH, security)** — `auth.internal.enabled` is dead config (declared `config.go:37`, default `true`, but `cmd/supervisor/main.go:78` never reads it) AND when `tokens_file` is set but the file is missing, `internal/auth/token.go:51-53` warns and accepts **ALL tokens** (silent auth bypass). Fix: (a) honor `Enabled` in `main.go` (skip/no-op `TokenValidator` when false); (b) when `enabled:true` and file missing, return `false, error` (accept-all only when explicitly disabled). Add tests.
- **B3 (MEDIUM, DoS)** — `internal/api/server.go:219` `io.ReadAll(r.Body)` is unbounded. Fix during **Phase 3 (T4)**: `io.LimitReader(r.Body, 1<<20)` (1 MB) or a configurable max.
- **B4 (MEDIUM)** — `cmd/supervisor/main.go:148-153` hardcodes `/etc/dagger-cache/tls/tls.{crt,key}` for cert-manager/external. Fix: add `tls.cert_path`/`tls.key_path` (or `certs_dir`) to `TLSConfig` with `SetDefault`.
- **B5 (LOW)** — `internal/fleet/stub.go:98-99` `nextIP` is shared across versions; after 256 total scale-ups it yields invalid `10.0.0.256`. Fix: `10.0.0.%d` with `%254+1` or per-version tracking. *(Stub/test only.)*
- **B6 (LOW)** — `internal/api/server.go:327` allocates a `ReverseProxy` per request. Fix during **Phase 3**: create proxies once at startup (matches simplifier rec #1/#6).
- **V1** — `config.app.yaml.sample` documents `cache.public_host` but not `cache.internal_addr` (which `wrapCacheProxy` depends on). Add it to the sample (Phase 9 / T13).
- **V2** — `internal/fleet/manager_test.go` exercises the stub provider directly rather than the `Manager.Acquire` sequencing; widen coverage during **Phase 8 (T26)**.
- **V3** — Live `config.app.yaml:19` carries phantom `server.ui_url`; T28 already targets removal — ensure the live file (not just the sample) is cleaned.

### Simplifier recommendations (Step 3 — advisory, fold into migration)
- Single shared reverse-proxy constructor (`newHertzProxy(...)`) for OTel/metrics/cache proxies (dedupes 3 handlers).
- Keep exactly two response helpers: `writeError(c *app.RequestContext, status, msg)` + `writeJSON(c, v)`.
- One `h.Use(...)` request-log middleware; drop per-handler method/path logging.
- Extract `queryAndWriteTraceLogs(traceID, c)` shared by `handleTracesLogs` + `handleLogsRoutes`.
- Settle the `observ` surface: `NewLogger`, `NewTestLogger`, `NewMetrics(reg)`; inject `*Metrics` everywhere; remove `promauto` globals.
- Create all reverse proxies once at startup, not per request.
- Ordering: SSE (Phase 4) strictly after Hertz handlers (Phase 3); Metrics DI (T18) as the last step of Phase 3 to avoid merge churn.

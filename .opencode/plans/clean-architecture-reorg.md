# Clean Architecture Reorganization — Implementation Plan

Module: `github.com/disaster/dagger-kubernetes` (unchanged)
Reference: https://reintech.io/blog/go-project-structure-2026-clean-architecture-best-practices

## 1. Goal

Reorganize the flat `internal/<pkg>` layout into layered clean architecture:

```
cmd/api/main.go                 (from cmd/supervisor — control plane API server)
cmd/ci/main.go                  (from cmd/dagger-cache-ci — CI wrapper CLI)
internal/
├── domain/                     pure entities + interfaces (stdlib ONLY)
├── service/                    business logic (imports domain, observ)
├── repository/                 infrastructure implementations (imports domain + drivers)
├── handler/                    Hertz HTTP/SSE/L4 handlers (imports service, repository, domain, observ)
└── observ/                     UNCHANGED (cross-cutting logging/metrics)
config/                         Go package: loader.go + config.app.yaml + config.app.yaml.sample
scripts/                        (renamed from hack/)
tests/integration/              black-box integration tests (from test/)
```

Dependency rule (enforced by imports): `handler → service → domain ← repository`.
`domain` imports stdlib only. `observ` remains a documented cross-cutting exception
(ADR-008): `service` and `handler` may inject `*observ.Metrics` / `*logrus.Logger`.

### Non-goals (explicit)
- No `pkg/`, `migrations/`, or root `api/` directories — no public utilities, no SQL store, no OpenAPI spec.
- No behavior changes: all HTTP routes, mTLS data plane, SSE, config keys/defaults, env vars unchanged.
- No deletion of dead code (see §12 note) — move as-is.
- Binary names unchanged: `supervisor` and `dagger-cache-ci` (via `-o` flags / `out:` entries).
- Helm chart untouched (renders config from values; no repo paths). `.golangci.yml` untouched.
- `.kilo/plans/` and existing ADRs 001–008 are historical records — do NOT edit their path references.

## 2. Resolved design decisions

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | `MintingCA` implementation lives in `repository/ca.go` (NOT service/) | CA providers construct it and `EmbeddedProvider` signs server certs with it; putting the impl in service/ would force a repository→service import (layering inversion). |
| D2 | Fleet naming helpers (`VersionSlug/StsName/PodName/ServiceName`) are EXPORTED functions in `domain/fleet.go` | `service.Manager` calls `PodName`/`StsName` (manager.go:116,147) and repository providers call all four; domain is the only layer both may import. Pure stdlib. |
| D3 | `k8s_integration_test.go` stays co-located: `internal/repository/k8s_provider_integration_test.go` (keeps `//go:build integration`, package `repository`) | It is white-box (uses unexported `engineLabelApp/engineLabelValue/enginePort`); cannot compile from `tests/` without exporting internals. |
| D4 | `cmd/dagger-cache-ci` → `cmd/ci` | It is a CI wrapper (execs `dagger`, emits annotations), not a background worker. |
| D5 | Single `domain` package → globally unique names: `FleetProvider`, `CAProvider`, `SessionStore`, `VersionResolver`, `TokenValidator`, `CacheBackend` (interface), `MintingCA` (interface) | `fleet.Provider`/`ca.Provider` would collide; `CacheConfig` is taken by the viper sub-struct. |
| D6 | Auth split: `domain.TokenValidator{ValidateToken(string)(string,error)}`; `extractToken` moves to `handler/auth.go`; handler calls new `authenticate(c)` helper | `ValidateRequest(*app.RequestContext)` cannot live behind a stdlib-only domain interface. Behavior preserved — see §13. |
| D7 | Telemetry clients injected into `handler.NewServer` as `domain.TraceRepository` / `domain.LogRepository` (constructed once in main), replacing per-request construction | Dependency inversion; matches article (handler must not new-up infrastructure per request). |
| D8 | `LiveHub`/`LiveClient` → `repository/live_hub.go`, consumed by handler as concrete `*repository.LiveHub` (constructed inside `NewServer` as today) | Keeps Hertz SSE types out of domain; handler may import repository. |
| D9 | `handler.NewServer` takes domain interfaces for `MintingCA/SessionStore/CacheBackend/VersionResolver/TokenValidator`, concrete `*service.Manager` | Article pattern: handler→service concrete; interfaces where a layer consumes an outer-layer implementation. |
| D10 | `cache.Backend` renamed `service.Cache` (not `Service`) | `service.Service` is meaningless; `Cache` is precise. |
| D11 | Config types → `domain/config.go`; Viper loader → root `config/loader.go` returning `*domain.Config`; YAML files → `config/` | Per article + user choice. Loader in public root package is acceptable (imports internal/domain legally). |

## 3. Baseline (do first)

1. `go build ./... && go test ./...` — confirm green before touching anything.
2. Record coverage baseline: `go test -cover ./internal/... ./test/...`.
3. All moves use `git mv` (or `cp` where noted) to preserve history. Everything lands as ONE commit; phases below are ordering for compile-while-you-go.

## 4. NEW FILES — `internal/domain/` (package `domain`, stdlib only)

### 4.1 `internal/domain/fleet.go`
- Imports: `fmt`, `strings`, `time`.
- Move verbatim from `internal/fleet/types.go`: `Replica`, `FleetInfo`.
- Move from `internal/fleet/manager.go`: `AcquireResult`.
- Interface (renamed from `fleet.Provider`):
```go
type FleetProvider interface {
    EnsureStatefulSet(version string, image string) error
    DeleteStatefulSet(version string) error
    EnsureService(version string) error
    DeleteService(version string) error
    GetReplicas(version string) ([]Replica, error)
    ScaleUp(version string, targetReplicas int) error
    ScaleDown(version string, ordinal int) error
    GetReadyReplicaIP(version string, podName string) (string, error)
    WaitForReady(version string, podName string) error
    GetEngineImage(version string) string
    AllVersions() ([]string, error)
}
```
- Exported naming helpers (from `internal/fleet/naming.go`, bodies unchanged, names capitalized):
```go
func VersionSlug(version string) string            // strings.ReplaceAll(version, ".", "-")
func StsName(version string) string                // fmt.Sprintf("dagger-engine-%s", VersionSlug(version))
func PodName(version string, ordinal int) string   // fmt.Sprintf("dagger-engine-%s-%d", VersionSlug(version), ordinal)
func ServiceName(version string) string            // fmt.Sprintf("dagger-engine-%s", VersionSlug(version))
```

### 4.2 `internal/domain/session.go`
- Imports: `time`.
- Move `Lease` verbatim from `internal/session/store.go`.
- New interface — union of methods used by production consumers (Manager: `PinnedSessionsOnReplica`, `Remove`, `List`; handler: `Register`, `Get`, `Touch`, `IncInFlight`, `DecInFlight`):
```go
type SessionStore interface {
    Register(certFP, version, replicaPod, instanceID, traceID string) *Lease
    Get(certFP string) (*Lease, error)
    Touch(certFP string) error
    IncInFlight(certFP string) error
    DecInFlight(certFP string) error
    Remove(certFP string)
    PinnedSessionsOnReplica(podName string) int
    List() []*Lease
}
```
- `ReapOrphans`, `Count`, `ListByVersion` stay concrete-only (test-only consumers).

### 4.3 `internal/domain/version.go`
- Imports: `fmt`, `regexp`, `strconv`, `strings`.
- Move from `internal/version/version.go`: `versionRe` (unexported), `Version` struct (KEEP FIELD ORDER `Major, Minor, Patch, Raw` — tests use positional literals), `Parse`, methods `Compare`, `MinorKey`, `Slug`, `CacheRefTag`, `String`.
- New interface (methods consumed by handler):
```go
type VersionResolver interface {
    IsAllowed(v *Version) bool
    ResolveMinimal(raw string) (*Version, error)
    Floor() *Version
    AllReleases() []*Version
}
```

### 4.4 `internal/domain/cache.go`
- Imports: none.
- Move `S3Ref` from `internal/cache/cache.go`.
- Minimal interface (handler only reads type/registry — `handleCacheInfo`):
```go
type CacheBackend interface {
    BackendType() string
    RegistryHost() string
}
```
- `CacheRefForVersion/BuildCacheConfig/BuildEngineJSON` stay concrete-only (no production callers; tested via concrete type). No backend-type constants exist today — do not invent them.

### 4.5 `internal/domain/ca.go`
- Imports: `crypto/tls`, `crypto/x509`, `fmt`.
- Move `SerializableCertificate` + its methods `Fingerprint()` and `TLSClientCertificate()` verbatim from `internal/ca/ca.go`.
- Interfaces:
```go
type MintingCA interface {
    MintClientCert(commonName string) (*SerializableCertificate, error)
    CertPool() *x509.CertPool
}

type CAProvider interface {
    MintingCA() (MintingCA, error)          // NOTE: returns interface, not *MintingCA
    ServerTLSCert() (tls.Certificate, error)
}
```
- `TLSCertificate()`/`EncodePEM()` stay concrete-only (used by tests/providers via concrete type).

### 4.6 `internal/domain/auth.go`
- Imports: none.
```go
type TokenValidator interface {
    ValidateToken(token string) (string, error)
}
```

### 4.7 `internal/domain/telemetry.go`
- Imports: `time`.
- Move DTOs verbatim (with JSON tags): `SpanNode`, `SpanLog`, `TraceInfo` (from `telemetry/traces.go`), `LogEntry` (from `telemetry/logs.go`), `MetricResult` (from `telemetry/metrics.go`).
- New interfaces:
```go
type TraceRepository interface {
    GetTrace(traceID string) (*TraceInfo, error)
}
type LogRepository interface {
    QueryTraceLogs(traceID string, start, end time.Time, limit int) ([]LogEntry, error)
}
```
- No interface for `MetricsClient` (zero production callers).

### 4.8 `internal/domain/config.go`
- Imports: `time`.
- Move verbatim from `internal/config/config.go` (all `mapstructure` tags preserved): `Config`, `ServerConfig`, `AuthConfig`, `InternalAuthConfig`, `OAuthConfig`, `TelemetryConfig`, `CacheConfig`, `S3Config`, `FleetConfig`, `CAConfig`, `TLSConfig`, `VersionConfig`, `CIConfig`, `GHAConfig`, `JenkinsConfig`, `DroneConfig`, `OTelConfig`.
- Edge case: `domain.ServerConfig` (viper) vs `handler.ServerConfig` (runtime wiring) — distinct packages, no collision; keep both names to minimize churn.

Compile checkpoint: `go build ./internal/domain/`.

## 5. NEW FILES — `config/` (root package)

### 5.1 `config/loader.go`
- `package config`
- Imports: `errors`, `fmt`, `io/fs`, `strings`, `time`; `github.com/spf13/viper`; `github.com/disaster/dagger-kubernetes/internal/domain`.
- `func Load(configFile string) (*domain.Config, error)` — body VERBATIM from `internal/config/config.go` `Load()` (every `v.SetDefault`, env prefix `DAGGER_CACHE`, replacer, `AutomaticEnv`, `ConfigFileNotFoundError`/`fs.ErrNotExist` handling); `var cfg domain.Config`.

### 5.2 Config YAML files (moved in Phase 9)
- `git mv config.app.yaml config/config.app.yaml`
- `git mv config.app.yaml.sample config/config.app.yaml.sample`
- Edit sample header comment: `internal/config/config.go` → `config/loader.go`.

Compile checkpoint: `go build ./config/`.

## 6. NEW FILES — `internal/service/` (package `service`)

### 6.1 `internal/service/fleet.go`
- Imports: `context`, `fmt`, `sort`, `time`; `github.com/sirupsen/logrus`; `internal/domain`; `internal/observ`.
- Move `Manager`, `ManagerConfig` from `fleet/manager.go`. Field types change: `provider domain.FleetProvider`, `sessions domain.SessionStore` (was concrete `*session.Store`). `metrics *observ.Metrics`, `logger *logrus.Logger` unchanged.
- `func NewManager(provider domain.FleetProvider, sessions domain.SessionStore, cfg ManagerConfig, logger *logrus.Logger, metrics *observ.Metrics) *Manager`.
- All methods move verbatim with these substitutions: `Replica`→`domain.Replica`, `FleetInfo`→`domain.FleetInfo`, `AcquireResult`→`domain.AcquireResult`, `podName(...)`→`domain.PodName(...)`, `stsName(...)`→`domain.StsName(...)`, `l.ReplicaPod`/`l.InFlight` on `*domain.Lease`.
- Keep: `Acquire`, `Unpin`, `GetVersionFleet`, `Sweep`, `sweepVersion`, `replicaHasActiveSessions`, `sortDescendingOrdinal`, `ScaleToZero`, `AllFleetInfo`, `observeReplicas` (nil-metrics guard preserved).

### 6.2 `internal/service/session.go`
- Imports: `fmt`, `sync`, `time`; `internal/domain`.
- Move `Store` from `session/store.go`: `leases map[string]*domain.Lease`. All 11 methods verbatim (`Register`, `Get`, `Touch`, `IncInFlight`, `DecInFlight`, `Remove`, `PinnedSessionsOnReplica`, `ReapOrphans`, `Count`, `List`, `ListByVersion`).
- Add compile-time assertion: `var _ domain.SessionStore = (*Store)(nil)`.
- TTL-expiry semantics in `Get` and `ReapOrphans` unchanged.

### 6.3 `internal/service/version.go`
- Imports: `fmt`, `sort`, `sync`, `time`; `internal/domain`.
- Move `Resolver` struct + `NewResolver(floor string, allowlist []string, releases map[string][]string) (*Resolver, error)` + `IsAllowed`, `ResolveMinimal`, `SetReleases`, `NeedsRefresh`, `Floor`, `AllReleases` — all use `domain.Version`/`domain.Parse`.
- `var _ domain.VersionResolver = (*Resolver)(nil)`.

### 6.4 `internal/service/cache.go`
- Imports: `encoding/json`, `fmt`, `strings`; `internal/domain`.
- Rename `Backend` → `Cache` (exported fields unchanged: `Type`, `Registry`, `PublicHost`, `S3 domain.S3Ref`). Constructed by struct literal in main/tests (no constructor needed).
- New getter methods for the interface: `func (b *Cache) BackendType() string { return b.Type }`, `func (b *Cache) RegistryHost() string { return b.Registry }`.
- Move `CacheRefForVersion(v *domain.Version)`, `BuildCacheConfig(v *domain.Version, mode string)`, `BuildEngineJSON(authToken string)` verbatim; also move `RegistryAuthEntry`, `EngineJSON` types here (JSON wire types, implementation detail).
- `var _ domain.CacheBackend = (*Cache)(nil)`.

### 6.5 `internal/service/auth.go`
- Imports: `fmt`, `os`, `strings`; `github.com/sirupsen/logrus`; `internal/domain`.
- Move `TokenValidator` struct (`TokensFile`, `Enabled`, `logger`), `NewTokenValidator`, `ValidateToken`, `checkTokenFile` verbatim.
- REMOVE `ValidateRequest` and `extractToken` (they move to handler — see §8.2).
- `var _ domain.TokenValidator = (*TokenValidator)(nil)`.

Compile checkpoint: `go build ./internal/service/`.

## 7. NEW FILES — `internal/repository/` (package `repository`)

### 7.1 `internal/repository/ca.go`
- Imports: `crypto`, `crypto/ecdsa`, `crypto/elliptic`, `crypto/rand`, `crypto/tls`, `crypto/x509`, `crypto/x509/pkix`, `encoding/pem`, `fmt`, `math/big`, `time`; `internal/domain`.
- Move from `ca/ca.go`: `MintingCA` struct (unexported fields `cert`, `key`, `certDER`, `clientCertTTL`), `NewMintingCA`, `NewMintingCAFromPEM`, `MintClientCert`, `CertPool`, `TLSCertificate`, `EncodePEM`, `parsePrivateKey` — verbatim.
- NEW method (extracted from `EmbeddedProvider.issueServerCert` crypto core; file I/O stays in the provider):
```go
// IssueServerCertificate signs a TLS server certificate with this CA.
// Pure crypto — the caller persists the returned PEM bytes.
func (ca *MintingCA) IssueServerCertificate(commonName, organization string, dnsNames []string, ttl time.Duration) (certPEM, keyPEM []byte, err error)
```
  Body: generate ECDSA P-256 key; random 128-bit serial; template `CommonName=commonName`, `Organization=[]string{organization}`, `NotBefore=now`, `NotAfter=now.Add(ttl)`, `KeyUsage=DigitalSignature|KeyEncipherment`, `ExtKeyUsage=[ServerAuth]`, `DNSNames=dnsNames`; sign with `ca.cert`/`ca.key`; PEM-encode cert (`"CERTIFICATE"`) and key via `x509.MarshalECPrivateKey` (`"EC PRIVATE KEY"` — keep exactly, `tls.X509KeyPair` compatibility). Wrap all errors with `%w`.
- `var _ domain.MintingCA = (*MintingCA)(nil)`.

### 7.2 `internal/repository/ca_providers.go`
- Imports: `crypto/tls`, `fmt`, `os`, `path/filepath`, `time`; `github.com/disaster37/goca`; `internal/domain`.
- Move `EmbeddedProvider`, `CertManagerProvider`, `ExternalProvider`, `fileExists` from `ca/providers.go`.
- Signature changes: all `MintingCA()` methods return `(domain.MintingCA, error)`; they call `NewMintingCAFromPEM` (now SAME package — no import needed).
- `EmbeddedProvider.issueServerCert` rewritten:
```go
func (p *EmbeddedProvider) issueServerCert(ca *MintingCA, certPath, keyPath string) (tls.Certificate, error) {
    certPEM, keyPEM, err := ca.IssueServerCertificate(
        "supervisor-server", "dagger-cache",
        []string{"localhost", "supervisor", "supervisor-control", "supervisor-control.dagger-cache.svc"},
        5*365*24*time.Hour)
    if err != nil {
        return tls.Certificate{}, fmt.Errorf("issue server cert: %w", err)
    }
    if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
        return tls.Certificate{}, fmt.Errorf("write server cert: %w", err)
    }
    if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
        return tls.Certificate{}, fmt.Errorf("write server key: %w", err)
    }
    return tls.X509KeyPair(certPEM, keyPEM)
}
```
  (Arguments replicate today's hardcoded values exactly — cert CN/subject/DNS names/TTL unchanged.)
- Keep `//nolint:gosec` comments on the `os.ReadFile` lines.
- `var _ domain.CAProvider = (*EmbeddedProvider)(nil)` (+ same for CertManager/External).

### 7.3 `internal/repository/k8s_provider.go`
- Imports: `context`, `fmt`, `math`, `strings`, `time`; `appsv1 "k8s.io/api/apps/v1"`, `corev1 "k8s.io/api/core/v1"`, `apierrors "k8s.io/apimachinery/pkg/api/errors"`, `"k8s.io/apimachinery/pkg/api/resource"`, `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, `"k8s.io/apimachinery/pkg/labels"`, `"k8s.io/apimachinery/pkg/util/intstr"`, `"k8s.io/client-go/kubernetes"`; `internal/domain`.
- Move from `fleet/k8s.go` verbatim: constants `engineLabelApp/engineLabelValue/engineLabelVersion/enginePort` (stay UNEXPORTED here), `K8sProviderConfig`, `K8sProvider`, `NewK8sProvider` (keep value-param `//nolint:gocritic` + defaulting), all methods.
- Substitutions: `stsName`→`domain.StsName`, `serviceName`→`domain.ServiceName`, `versionSlug`→`domain.VersionSlug` (in `extractOrdinal`), `Replica`→`domain.Replica`, keep `//nolint` comments.
- `var _ domain.FleetProvider = (*K8sProvider)(nil)`.

### 7.4 `internal/repository/stub_provider.go`
- Imports: `fmt`, `sort`, `sync`, `time`; `internal/domain`.
- Move from `fleet/stub.go`: `ReplicaState` + consts, `StubProvider`, `stubSTS` (`replicasM map[string]*domain.Replica`), `NewStubProvider`, all methods; `podName(...)`→`domain.PodName(...)`.
- `var _ domain.FleetProvider = (*StubProvider)(nil)`.

### 7.5 `internal/repository/live_hub.go`
- Imports: `encoding/json`, `sync`, `time`; `github.com/cloudwego/hertz/pkg/app`; `github.com/cloudwego/hertz/pkg/protocol/sse`.
- Move `LiveClient`, `LiveHub`, `NewLiveHub`, `Subscribe`, `Unsubscribe`, `BroadcastSpanUpdate`, `writePump`, `ClientCount`, `NewLiveClient`, `Done` verbatim from `telemetry/live.go`. No domain dependency.

### 7.6 `internal/repository/trace_store.go`
- Imports: `encoding/json`, `fmt`, `net/http`, `regexp`, `strconv`, `time`; `internal/domain`.
- Move from `telemetry/traces.go`: `hexTraceID` (unexported; shared with log_store.go — same package), `SpanTreeReconstructor`, `NewSpanTreeReconstructor`, `GetTrace`, `reconstruct`, `extractSpans`, `mapToSpanNode`.
- Type substitutions: `*TraceInfo`→`*domain.TraceInfo`, `*SpanNode`→`*domain.SpanNode`, `SpanLog`→`domain.SpanLog`.
- `var _ domain.TraceRepository = (*SpanTreeReconstructor)(nil)`.

### 7.7 `internal/repository/log_store.go`
- Imports: `encoding/json`, `fmt`, `net/http`, `net/url`, `strings`, `time`; `internal/domain`.
- Move from `telemetry/logs.go`: `LogsClient`, `NewLogsClient`, `QueryTraceLogs` (returns `([]domain.LogEntry, error)`), `parseNanos`, `sanitizeLogQLValue`.
- `var _ domain.LogRepository = (*LogsClient)(nil)`.

### 7.8 `internal/repository/metrics_store.go`
- Imports: `encoding/json`, `fmt`, `net/http`, `net/url`, `time`; `internal/domain`.
- Move from `telemetry/metrics.go`: `MetricsClient`, `NewMetricsClient`, `InstantQuery`, `RangeQuery`, `doQuery` (returns `([]domain.MetricResult, error)`). No interface assertion (no consumers).

Compile checkpoint: `go build ./internal/repository/`.

## 8. NEW FILES — `internal/handler/` (package `handler`)

### 8.0 FIRST: copy embedded UI assets
`cp -r internal/api/ui-dist internal/handler/ui-dist` (COPY, not move — old `internal/api/ui.go` must keep compiling until Phase 8 deletion; the old copy is removed in Phase 8).

### 8.1 `internal/handler/server.go`
- Imports: `context`, `crypto/tls`, `encoding/json`, `fmt`, `io`, `net`, `net/url`, `strings`, `time`; `github.com/cloudwego/hertz/pkg/app`, `.../app/server`, `.../common/adaptor`, `.../protocol`, `.../protocol/consts`; `github.com/hertz-contrib/reverseproxy`; `github.com/prometheus/client_golang/prometheus/promhttp`; `github.com/sirupsen/logrus`; `internal/domain`, `internal/observ`, `internal/repository`, `internal/service`.
- Move from `api/server.go`: consts `maxRequestBodyBytes`, `otelSignalKey`, `otelErrorKey`; `EngineRequest`; `ErrorResponse`; `ServerConfig` (keep unused `TokensFile` field — do not delete); `Start`, `configure`, `buildProxies`, `newHertzProxy`, `Shutdown`, `handleHealthz`, `handleReadyz`, `handleEngines`, `extractVersion`, `handleOTel`, `handleAdminVersions`, `handleFleetInfo`, `handleCacheInfo`, `handleDataConn`, `handleNoRoute`, `cacheHostMiddleware`, `serveCacheHost`, `requestLog`, `writeError`, `writeJSON`.
- `EngineSpecResponse.Cert` becomes `*domain.SerializableCertificate`.
- New `Server` struct fields / `NewServer` signature:
```go
type Server struct {
    cfg             *ServerConfig
    logger          *logrus.Logger
    metrics         *observ.Metrics
    mintingCA       domain.MintingCA
    fleetManager    *service.Manager
    sessions        domain.SessionStore
    cacheBackend    domain.CacheBackend
    versionResolver domain.VersionResolver
    liveHub         *repository.LiveHub
    tokenValidator  domain.TokenValidator
    traces          domain.TraceRepository
    logs            domain.LogRepository
    hertz           *server.Hertz
    tlsListener     net.Listener
    otelProxy, victoriaProxy, cacheProxy *reverseproxy.ReverseProxy
}

func NewServer(
    cfg *ServerConfig,
    logger *logrus.Logger,
    metrics *observ.Metrics,
    mintingCA domain.MintingCA,
    fleetManager *service.Manager,
    sessions domain.SessionStore,
    cacheBackend domain.CacheBackend,
    versionResolver domain.VersionResolver,
    tokenValidator domain.TokenValidator,
    traces domain.TraceRepository,
    logs domain.LogRepository,
) *Server   // liveHub: repository.NewLiveHub() inside, as today
```
- Mechanical substitutions in moved handlers: every `if _, err := s.tokenValidator.ValidateRequest(c); err != nil` → `if _, err := s.authenticate(c); err != nil`; `handleCacheInfo` reads `s.cacheBackend.BackendType()` / `s.cacheBackend.RegistryHost()`; `extractVersion` returns `(*domain.Version, error)`; `handleDataConn` lease lookups via `domain.SessionStore` (semantics unchanged); `handleFleetInfo` serializes `[]domain.FleetInfo`.

### 8.2 `internal/handler/auth.go`
- Imports: `encoding/base64`, `fmt`, `strings`; `github.com/cloudwego/hertz/pkg/app`.
- Move `extractToken(c *app.RequestContext) (string, error)` verbatim from `auth/token.go` (Bearer primary, Basic fallback, `fmt.Sprintf`-free — already compliant; keep `%w` wraps).
- NEW helper on Server:
```go
// authenticate extracts the bearer/basic token and validates it. Extraction
// failures degrade to an empty token so that disabled-auth mode accepts
// requests exactly as ValidateRequest did.
func (s *Server) authenticate(c *app.RequestContext) (string, error) {
    token, err := extractToken(c)
    if err != nil {
        token = ""
    }
    return s.tokenValidator.ValidateToken(token)
}
```
- Behavior-preservation table (must hold; verified against call sites — all discard the returned subject): see §13.

### 8.3 `internal/handler/traces.go`
- Imports: `context`; `github.com/cloudwego/hertz/pkg/app`, `.../protocol/consts`; `internal/repository`.
- Move `handleTracesList`, `handleTracesDetail`, `handleTracesLogs`, `handleTracesLive` (auth swaps per §8.1).
- `handleTracesDetail`: replace `telemetry.NewSpanTreeReconstructor(s.cfg.TempoURL)` + `reconstructor.GetTrace` with `s.traces.GetTrace(traceID)`.
- `handleTracesLive`: `client := repository.NewLiveClient(c, traceID)`; `s.liveHub.Subscribe/Unsubscribe` — otherwise verbatim (SSE headers etc.).

### 8.4 `internal/handler/logs.go`
- Imports: `context`, `strings`, `time`; hertz `app`, `consts`.
- Move `queryAndWriteTraceLogs`, `handleLogsRoutes` (auth swap). Replace `telemetry.NewLogsClient(s.cfg.LokiURL)` + `logsClient.QueryTraceLogs` with `s.logs.QueryTraceLogs(traceID, start, end, 1000)`.

### 8.5 `internal/handler/metrics.go`
- Imports: `context`; hertz `app`, `consts`.
- Move `handleMetricsProxy` verbatim (auth swap).

### 8.6 `internal/handler/ui.go`
- Imports: `context`, `embed`, `io`, `io/fs`, `mime`, `path/filepath`, `strings`; hertz `app`, `consts`.
- Move `serveUI`, `serveFile`, `contentTypeFor` verbatim. Keep `//go:embed all:ui-dist` — requires §8.0 done first.

Compile checkpoint: `go build ./internal/handler/`.

## 9. NEW ENTRY POINTS — `cmd/`

### 9.1 `cmd/api/main.go` (from `cmd/supervisor/main.go`)
- `package main`.
- Imports: `context`, `fmt`, `log`, `os`, `os/signal`, `strings`, `syscall`, `time`; `github.com/prometheus/client_golang/prometheus`; `github.com/sirupsen/logrus`; `github.com/urfave/cli/v2`; `corev1 "k8s.io/api/core/v1"`; `"k8s.io/client-go/kubernetes"`, `"k8s.io/client-go/rest"`, `"k8s.io/client-go/tools/clientcmd"`; `github.com/disaster/dagger-kubernetes/config`; `internal/domain`; `internal/handler`; `internal/observ`; `internal/repository`; `internal/service`.
- `cli.App{Name: "supervisor", ...}` — keep name/usage. Flag default changes: `Value: "config/config.app.yaml"`.
- `run()` wiring changes (everything else verbatim, incl. sweep goroutine + signal handling):
  - `cfg, err := config.Load(...)` → `*domain.Config`
  - `selectTLSProvider(cfg *domain.Config) (domain.CAProvider, error)` — cases return `repository.NewEmbeddedProvider(...)`, `repository.NewCertManagerProvider(...)`, `repository.NewExternalProvider(...)`.
  - `serverMintingCA, err := tlsProvider.MintingCA()` → now `domain.MintingCA`.
  - `versionResolver, err := service.NewResolver(cfg.Version.Floor, cfg.Version.Allowlist, nil)`
  - `sessions := service.NewStore(cfg.LeaseTTL)`
  - `cacheBackend := &service.Cache{Type: cfg.Cache.Backend, Registry: cfg.Cache.Registry, PublicHost: cfg.Cache.PublicHost, S3: domain.S3Ref{Bucket: cfg.Cache.S3.Bucket, Region: cfg.Cache.S3.Region}}`
  - `tokenValidator := service.NewTokenValidator(cfg.Auth.Internal.TokensFile, cfg.Auth.Internal.Enabled, logger)`
  - `createProvider(cfg *domain.Config, logger *logrus.Logger) domain.FleetProvider` — body unchanged except `fleet.K8sProviderConfig`→`repository.K8sProviderConfig`, `fleet.NewK8sProvider`→`repository.NewK8sProvider`, `fleet.NewStubProvider`→`repository.NewStubProvider`.
  - `fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{...}, logger, metrics)`
  - NEW: `traces := repository.NewSpanTreeReconstructor(cfg.Telemetry.TempoURL)`; `logsClient := repository.NewLogsClient(cfg.Telemetry.LokiURL)`
  - `server := handler.NewServer(&handler.ServerConfig{...same fields...}, logger, metrics, serverMintingCA, fleetManager, sessions, cacheBackend, versionResolver, tokenValidator, traces, logsClient)`
- `newK8sClientset`, `parseTolerations` move verbatim.

### 9.2 `cmd/ci/main.go` (from `cmd/dagger-cache-ci/main.go`)
- Verbatim copy (no internal imports — stdlib + urfave/cli only). Keep `cli.App{Name: "dagger-cache-ci"}`.

Compile checkpoint: `go build ./cmd/api ./cmd/ci`.

## 10. TEST MIGRATION

Unit tests stay white-box (same package as their subject). Exact mapping:

| Old file | New file | Notes |
|---|---|---|
| `internal/version/version_test.go` | SPLIT → `internal/domain/version_test.go` (`TestParse`, `TestVersionCompare`, `TestVersionSlug`; package `domain`) and `internal/service/version_test.go` (`TestResolverFloor`, `TestResolverAllowlist`, `TestResolveMinimal`; package `service`, `domain.Parse`) | Positional literal `&Version{0, 21, 4, "v0.21.4"}` stays valid (field order preserved). |
| `internal/session/store_test.go` | `internal/service/session_test.go` | package `service`; `Lease` refs → `domain.Lease`. |
| `internal/cache/cache_test.go` | `internal/service/cache_test.go` | package `service`; `version.Parse`→`domain.Parse`; `Backend`→`Cache`. |
| `internal/auth/token_test.go` | SPLIT → `internal/service/auth_test.go` (the six `TestValidateToken*` tests; keep `observ.NewTestLogger`) and `internal/handler/auth_test.go` (REWRITTEN, see below) | |
| `internal/fleet/manager_test.go` | `internal/service/fleet_test.go` | package `service`; `NewStubProvider`→`repository.NewStubProvider` (add import); `session.NewStore`→`NewStore` (same package now); `ManagerConfig`/`NewManager` same package. |
| `internal/fleet/k8s_test.go` | SPLIT → `internal/repository/k8s_provider_test.go` (everything EXCEPT `TestK8sProviderWithManagerIntegration`; drop the `internal/session` import) and `internal/service/k8s_manager_test.go` (that one test; package `service`, imports `repository` for `NewK8sProvider`/`K8sProviderConfig`, `domain`, k8s `fake` clientset; `NewManager`/`NewStore` same package) | Test-only service→repository import is allowed; production service never imports repository. |
| `internal/fleet/k8s_integration_test.go` | `internal/repository/k8s_provider_integration_test.go` | KEEP `//go:build integration` + `// +build integration`; package `repository`; `session.NewStore`→`service.NewStore`, `NewManager`→`service.NewManager`, `ManagerConfig`→`service.ManagerConfig`, `*AcquireResult`→`*domain.AcquireResult`; `stsName`/`serviceName` → `domain.StsName`/`domain.ServiceName`; label/port constants now same-package. |
| `internal/ca/ca_test.go` | SPLIT → `internal/repository/ca_test.go` (`TestMintingCARoundTrip`, `TestMintingCAPEMRoundTrip`, `TestSerializableCertificateToTLS`) and `internal/repository/ca_providers_test.go` (`TestEmbeddedProviderCreateCA`) | Both package `repository` — no import changes beyond `domain` where referenced. |
| `internal/telemetry/telemetry_test.go` | `internal/repository/telemetry_test.go` | 1:1 (white-box access to `lokiURL`/`victoriaURL`/`tempoURL` preserved; all telemetry impls land in same package). |
| `internal/config/config_test.go` | `config/loader_test.go` | 1:1 (package `config`; only uses `Load` + field access). |
| `internal/api/api_test.go` | `internal/handler/server_test.go` | package `handler`; `newTestServer` rebuild: `repository.NewMintingCA(2*time.Hour)`, `service.NewResolver("v0.19.0", nil, nil)`, `service.NewStore(2*time.Minute)`, `repository.NewStubProvider()`, `service.NewManager(...)`, `&service.Cache{Type:"registry", Registry:"cache.reg/dagger-cache"}`, `service.NewTokenValidator("", false, logger)`, plus NEW args `repository.NewSpanTreeReconstructor("")`, `repository.NewLogsClient("")`. `newTestEngine` unchanged. `EngineSpecResponse` same name. |
| `internal/observ/observ_test.go` | UNCHANGED | |
| `test/integration_test.go` | `tests/integration/api_test.go` | package `integration` (black-box). Import swaps: `api`→`handler`, `auth/cache/version`→`service`, `ca/fleet`→`repository`, `session`→`service`, +`domain`. Construction as in handler test; `mintingCA.TLSCertificate()` still works (concrete `*repository.MintingCA`). `api.EngineSpecResponse`→`handler.EngineSpecResponse`, `api.ServerConfig`→`handler.ServerConfig`, `fleet.ManagerConfig`→`service.ManagerConfig`. |
| `dagger/deps/lib/pipeline/*_test.go` | UNCHANGED | separate Dagger module. |

`internal/handler/auth_test.go` rewrite detail:
- `TestAuthenticateDisabledAcceptsNoHeader`: build minimal `Server{tokenValidator: service.NewTokenValidator("", false, observ.NewTestLogger())}`; `c := ut.CreateUtRequestContext("POST", "/v1/engines", nil)`; `_, err := s.authenticate(c)` → expect nil error (subject is no longer `"no-auth"` — it is `""`; assert no error only).
- `TestExtractTokenSchemes`: table test calling `extractToken(c)` directly — bearer → `"test-token"`, basic → `"test-token"`, missing header → error, `Digest abc` → error. (Validator not involved.)

## 11. DELETIONS (Phase 8 — only after all of the above compile and tests pass)

Delete entire directories: `internal/api/` (including the now-stale original `ui-dist/`), `internal/auth/`, `internal/ca/`, `internal/cache/`, `internal/config/`, `internal/fleet/`, `internal/session/`, `internal/telemetry/`, `internal/version/`, `cmd/supervisor/`, `cmd/dagger-cache-ci/`, `test/` (after git-mv of its only file), `hack/` (after git-mv).

## 12. DIRECTORY RENAMES / MOVES

- `git mv hack scripts` (keeps `dagger-cache.sh`, `update-helm-docs.sh`; both already path-agnostic — `update-helm-docs.sh` resolves ROOT from its own location).
- `git mv test/integration_test.go tests/integration/api_test.go` (create `tests/integration/` first).
- `git mv config.app.yaml config/` and `git mv config.app.yaml.sample config/` (after §5.1 exists).
- Dead-code note (move, do NOT delete): `Manager.Unpin`/`ScaleToZero`, `Store.ReapOrphans`/`Count`/`ListByVersion`, `Resolver.SetReleases`/`NeedsRefresh`, `MetricsClient`, `LiveHub.BroadcastSpanUpdate`/`ClientCount`, `handler.ServerConfig.TokensFile` have few/no production callers but are exercised by tests or kept for API surface.

## 13. BEHAVIOR PRESERVATION — auth truth table

All 14 `ValidateRequest` call sites discard the returned subject (`if _, err := ...`), so subject changes are invisible.

| Scenario | Old (`ValidateRequest`) | New (`authenticate` = extract→ValidateToken) | Status |
|---|---|---|---|
| auth disabled, no header | accept, subject `"no-auth"` | extract err → `""` → `ValidateToken("")` accepts | equivalent |
| auth disabled, malformed header | accept | extract err → `""` → accept | equivalent |
| auth enabled, no header | 401 (`missing authorization`) | `""` → `ValidateToken("")` → `"empty token"` err → 401 | equivalent |
| auth enabled, Bearer valid/invalid | pass/reject via tokens file | identical path | equivalent |
| auth enabled, Basic fallback | username-as-token | identical (extractToken moved verbatim) | equivalent |
| auth enabled, missing tokens file | fail closed (error) | unchanged (`checkTokenFile` verbatim) | equivalent |

## 14. RIPPLE UPDATES (Phase 10)

1. **`Dockerfile`** (root):
   - `COPY --from=ui-builder /ui/dist ./internal/handler/ui-dist/`
   - `go build ... -o /out/supervisor ./cmd/api/`
   - ci-builder: `go build ... -o /out/dagger-cache-ci ./cmd/ci/`
   - `COPY config/config.app.yaml.sample /etc/dagger-cache/config.app.yaml.sample`
   - ENTRYPOINT/CMD unchanged (`/usr/local/bin/supervisor`, `--config=/etc/dagger-cache/config.app.yaml`).
2. **`deploy/docker/Dockerfile`**: `go build ... -o /supervisor ./cmd/api/`. (CMD runtime path is helm-mounted; unchanged.)
3. **`.github/workflows/release.yml`**: `hack/update-helm-docs.sh` → `scripts/update-helm-docs.sh`; release builds `./cmd/ci/` and `./cmd/api/` (output names `dagger-kubernetes-ci-*` / `supervisor-*` unchanged).
4. **`.github/workflows/ci.yml`**: no changes (invokes the Dagger module).
5. **`dagger/main.go`** (lines 33–34): `{main: "./cmd/api/", out: "bin/supervisor"}`, `{main: "./cmd/ci/", out: "bin/dagger-cache-ci"}`.
6. **`AGENTS.md`**: rewrite "Project structure" block to the new layout (`cmd/api`, `cmd/ci`, `internal/domain|service|repository|handler|observ`, `config/`, `scripts/`, `tests/`); update the DI example types (`service.Manager`, `domain.SessionStore`); update "Documentation maintenance" reference `internal/config/config.go` → `config/loader.go`.
7. **`README.md`** (root): structure table rows → `cmd/api`, `cmd/ci` + `scripts/dagger-cache.sh`, `internal/` layer description, add `config/`.
8. **`docs/README.md`**: lines ~7, 178, 248, 324–325, 366, 545, 554, 618–619, 646 — `cmd/supervisor`→`cmd/api`, `cmd/dagger-cache-ci`→`cmd/ci`, `hack/`→`scripts/`, `internal/config/config.go`→`config/loader.go`, `internal/fleet/k8s.go`→`internal/repository/k8s_provider.go`, `internal/api`→`internal/handler`, `test/integration_test.go`→`tests/integration/api_test.go`; build commands updated.
9. **`CONTRIBUTING.md`**: structure section (~L159–166) to new layout; L184 sync-rule path → `config/loader.go`; L209 table row; L222–226 `hack/dagger-cache.sh` → `scripts/dagger-cache.sh` (both occurrences incl. the `cmp` line vs `ci-integrations/gha/dagger-cache.sh`).
10. **`DAGGER.md`** L65: `--main ./cmd/supervisor` → `--main ./cmd/api`.
11. **`config/config.app.yaml.sample`** header comment: `internal/config/config.go` → `config/loader.go`.
12. **NEW `docs/design/ADR-009-clean-architecture-layering.md`**: record layer rules, the D1–D11 decisions from §2, the `observ` exception, and the repository placement of `MintingCA`. Old ADRs untouched (historical).
13. **Helm chart, `.golangci.yml`, `go.mod`, `ci-integrations/`, `ui/`**: verify-only, no changes.

## 15. IMPLEMENTATION PHASES (compile-while-you-go)

1. **Phase 0 — Baseline:** build + full test run green; record coverage.
2. **Phase 1 — domain:** create §4.1–4.8. `go build ./internal/domain/ && go vet ./internal/domain/`.
3. **Phase 2 — config loader:** §5.1. `go build ./config/`.
4. **Phase 3 — service:** §6.1–6.5. `go build ./internal/service/`.
5. **Phase 4 — repository:** §7.1–7.8. `go build ./internal/repository/`.
6. **Phase 5 — handler:** §8.0 (copy ui-dist), then §8.1–8.6. `go build ./internal/handler/`.
7. **Phase 6 — entry points:** §9.1–9.2. `go build ./...` (old + new coexist; nothing imports handler yet except new cmds).
8. **Phase 7 — tests:** apply §10 mapping; `go test ./...` (old packages still present — both old and new tests pass).
9. **Phase 8 — delete old:** §11 (includes removing original `internal/api/ui-dist`). `go build ./... && go test ./...`.
10. **Phase 9 — renames/moves:** §12 (`hack`→`scripts`, `test`→`tests`, YAML files→`config/`).
11. **Phase 10 — ripple:** §14 items 1–12.
12. **Phase 11 — validation:** §16, full checklist.

## 16. VALIDATION CHECKLIST

- [ ] `gofmt -l .` empty; `goimports -local github.com/disaster/dagger-kubernetes` clean
- [ ] `go build ./...` and `go vet ./...` pass
- [ ] `go test ./...` passes (unit + `tests/integration` black-box suite)
- [ ] `go test -tags integration ./internal/repository/` — documented as requiring a live cluster (KUBECONFIG); run when available; must compile regardless
- [ ] `golangci-lint run` clean (same linter set as before)
- [ ] Coverage per package ≥ baseline, targeting 100% (AGENTS.md); tests use `observ.NewTestLogger()` / `logrus.New()`+`io.Discard` only — no testify
- [ ] `grep -RnE "internal/(api|auth|ca|cache|config|fleet|session|telemetry|version)|cmd/supervisor|cmd/dagger-cache-ci|hack/" --include='*.go' --include='*.yml' --include='*.yaml' --include='*.md' --include='Dockerfile' .` → matches ONLY in `.git/`, `.kilo/plans/` (historical), `docs/design/ADR-00[1-8]*` (historical), this plan file
- [ ] `docker build -f Dockerfile .` succeeds (ui-dist embed path + both binaries)
- [ ] `helm lint deploy/helm/dagger-kubernetes` passes (unchanged)
- [ ] Binary identities: `go run ./cmd/api --help` prints name `supervisor`; `go run ./cmd/ci --help` prints name `dagger-cache-ci`
- [ ] `config/config.app.yaml.sample` reflects every key/default in `config/loader.go` (doc-maintenance rule)
- [ ] No circular imports: `go build` is proof; additionally verify `internal/domain` imports stdlib only (`go list -deps internal/domain | grep -v '^[a-z]'` shows no third-party)
- [ ] All errors wrapped with `%w`; all strings via `fmt.Sprintf`; all logging via logrus `WithFields`/`WithError` (convention scan on touched files)

## 17. RISKS & FAILURE MODES

| Risk | Mitigation |
|---|---|
| `//go:embed all:ui-dist` fails when handler compiles | Copy ui-dist into `internal/handler/` BEFORE creating `handler/ui.go` (Phase 5 step 0); delete original only in Phase 8. |
| Auth behavior drift (disabled mode / malformed headers) | §13 truth table + dedicated `handler/auth_test.go` cases; all call sites discard the subject. |
| Prometheus double-registration panics in tests | Keep `observ.NewMetrics(nil)` pattern in every test (unchanged). |
| Positional `Version` literal breaks | Field order `Major, Minor, Patch, Raw` preserved in domain. |
| Interface satisfaction silently breaks | Compile-time `var _ domain.X = (*Y)(nil)` assertions in every implementation file. |
| Default config path change surprises devs (`config.app.yaml` → `config/config.app.yaml`) | Update CLI flag default, docs, sample location in the same commit; Docker CMD runtime path unchanged. |
| repository→service import temptation | Forbidden in production code by this plan; only TEST files in `service/` may import `repository` (for `StubProvider`/`K8sProvider` concrete test targets). |
| `hexTraceID` needed by both trace and log stores | Same `repository` package — shared unexported var works as today. |
| Git history loss | `git mv` for all moves/renames. |
| Dagger CI module builds stale cmd paths | `dagger/main.go` updated in Phase 10; `ci.yml` runs it, so CI itself validates. |

# Plan: Enterprise environment enhancements for Dagger engine scheduling

Goal: inject proxy env vars, a custom CA bundle, and a generated Dagger
`engine.toml` (debug, registry mirrors, log format) into Dagger engine
StatefulSet pods, driven by supervisor configuration; make the supervisor's
own log format configurable with `json` as the default. Additionally support
env vars sourced from Kubernetes Secrets (`fleet.engine_extra_env_from`) so
authenticated-proxy credentials (e.g. `HTTP_PROXY` containing user:pass)
never appear in plaintext config, while keeping the literal
`fleet.engine_extra_env` for non-sensitive values.

Module: `github.com/disaster/dagger-kubernetes`. All conventions per
`AGENTS.md` (fmt.Sprintf only, logrus, viper defaults, stdlib `testing`,
table-driven tests, 100% coverage target, docs updated in the same change).

---

## 1. Verified context (corrections to the original brief)

Verified against the code on 2026-08-10. Where the brief and the code
disagree, the code wins:

1. `internal/repository/k8s_provider.go` `K8sProviderConfig` fields have NO
   `Engine` prefix (`Namespace`, `ImageRegistry`, `StorageClass`, ...,
   `Tolerations []corev1.Toleration`, `ExtraArgs`, `PullPolicy`,
   `Privileged`). New fields follow the same unprefixed style.
2. `cmd/api/main.go` `createProvider()` converts `FleetConfig` (prefixed
   fields) → `K8sProviderConfig` (unprefixed), including
   `parseTolerations([]string) []corev1.Toleration`. New fields wire the same
   way.
3. `observ.NewLogger(level string)` ALREADY uses `logrus.JSONFormatter`
   (`internal/observ/logger.go`). Requirement "default log format json" is
   implemented as: new `log_format` config key (default `json`), signature
   becomes `NewLogger(level, format string)`, supporting `json`/`text`.
4. Dagger engines (v0.19–v0.21, the versions this platform admits per
   `version.floor`/`allowlist`) automatically read the legacy BuildKit-style
   config at **`/etc/dagger/engine.toml`** inside the container
   (docs.dagger.io "Engine configuration": mount example
   `-v $PWD/engine.toml:/etc/dagger/engine.toml`; `engine.json` is preferred
   but `engine.toml` remains supported and some options are TOML-exclusive).
   Consequences:
   - The file MUST be named `engine.toml` (NOT `dagger.toml`).
   - NO extra engine arg (`--config`) or env var is needed; mounting at the
     well-known path is sufficient.
   - TOML supports `debug = true` and `[registry."<host>"] mirrors = [...]`
     exactly as required. `[log] format = "..."` is emitted as specified by
     the requirement (unknown sections are ignored by the engine's TOML
     parser if not recognized — harmless).
5. `/etc/dagger` is already a whole-directory mount of the Secret volume
   `engine-config` (secret `engine-registry-auth`). The new `engine.toml`
   must therefore be mounted from a second volume using `subPath` — this
   coexists with the directory mount.
6. RBAC (`deploy/k8s/namespace-rbac.yaml`) already grants
   `configmaps`/`secrets` CRUD — no RBAC change needed.
7. `pelletier/go-toml/v2` is only an INDIRECT dep (via viper). TOML is
   rendered by hand (`fmt.Sprintf` + small escaper) — no new dependency.
8. `internal/repository/stub_provider.go` and
   `internal/handler/test_helper_test.go` need NO changes: the stub renders
   no pods and takes no config; the `domain.FleetProvider` interface is
   unchanged.

---

## 2. Design decisions

| # | Decision | Choice | Rationale |
|---|----------|--------|-----------|
| D1 | Proxy env vars | Generic `map[string]string` `fleet.engine_extra_env`, injected as literal env vars (sorted by name) | Covers `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` + lowercase variants and any future env (e.g. `KUBERNETES_SERVICE_HOST`). Mirrors existing `engine_extra_args` naming. Maps can't be set via `DAGGER_CACHE_` env overrides — same accepted limitation as `engine_node_selector`. |
| D2 | CA cert source | Reference an EXISTING Secret by name: `fleet.engine_ca_secret` + key `fleet.engine_ca_secret_key` (default `ca.crt`). Mounted read-only at fixed path `/etc/ssl/certs/custom-ca.pem`; env `SSL_CERT_FILE` and `NODE_EXTRA_CA_CERTS` point to it | K8s-native (operators/GitOps already manage CA secrets, e.g. from cert-manager). Fixed well-known path + env vars satisfy the requirement; no configurable mount path (escape hatch is `engine_extra_env`). Volume uses `Items` to normalize any key name to file `ca.crt`. NOT marked `Optional`: a missing secret must fail the pod loudly, not silently run without the CA while `SSL_CERT_FILE` points at a nonexistent file. |
| D3 | Dagger config | STRUCTURED fields in `FleetConfig` (`engine_debug`, `engine_log_format`, `engine_registry_mirrors`); the K8s provider renders TOML and stores it in a supervisor-managed ConfigMap `dagger-engine-config` (key `engine.toml`), ensured on every `EnsureStatefulSet`, mounted via subPath at `/etc/dagger/engine.toml` | Type-safe, validated, deterministic. Fleet-wide (all versions share one ConfigMap — engine config is version-independent here). No raw-TOML passthrough (would defeat validation). |
| D4 | Supervisor log format | Top-level `log_format` config (default `json`); `observ.NewLogger(level, format)` | Supervisor-wide concern, not per-fleet. Engine log format is separate and lives in `engine.toml` (`[log] format`). |
| D5 | Empty TOML case | If rendered TOML is empty (debug=false, log format `""`, no mirrors): no ConfigMap volume/mount, and a stale ConfigMap is deleted best-effort | Escape hatch back to the exact pre-change pod spec. Default config renders non-empty TOML (`[log] format = "json"`), so by default every engine gets the mount — this is the intended behavior change. |
| D6 | Validation location | Fail-fast in `createProvider` (package main) BEFORE provider construction; `createProvider` gains an `error` return | Duplicate container env names are rejected by K8s at STS admission; catching them at supervisor startup gives an actionable error. Internal function — signature change is safe. |
| D7 | Secret-sourced env vars | New map `fleet.engine_extra_env_from` of type `map[string]domain.EnvVarSource` (struct fields `SecretName`, `Key`; map key = env var name). Each entry is injected as `corev1.EnvVar{Name, ValueFrom.SecretKeyRef}` on the engine container, sorted by name, after the literal `engine_extra_env` entries. `SecretKeyRef` is NOT marked `Optional` | Authenticated proxies require credentials inside `HTTP_PROXY`/`HTTPS_PROXY`; plaintext config (config file, Helm values, git) is unacceptable. K8s SecretKeyRef is the native mechanism and needs no extra RBAC (the kubelet resolves refs, not the supervisor SA). The type lives in `domain` (stdlib-only per the dependency rule) and is REUSED as-is by `K8sProviderConfig.ExtraEnvFrom` — repository already imports domain, so a separate `repository.EnvVarSource` would only add conversion boilerplate. `envFrom`/whole-secret injection and ConfigMap/FieldRef sources are out of scope. |

---

## 3. Ordered implementation tasks

### Task 1 — `internal/domain/config.go`: config structs

Add `LogFormat` to `Config` (next to `LogLevel`, line 16):

```go
type Config struct {
    // ... unchanged fields ...
    LogLevel  string          `mapstructure:"log_level"`
    LogFormat string          `mapstructure:"log_format"` // "json" (default) | "text"
    // ... unchanged fields ...
}
```

Append to `FleetConfig` (after `EnginePrivileged`, line 108):

```go
    EngineExtraEnv        map[string]string   `mapstructure:"engine_extra_env"`
    EngineCASecret        string              `mapstructure:"engine_ca_secret"`
    EngineCASecretKey     string              `mapstructure:"engine_ca_secret_key"`
    EngineDebug           bool                `mapstructure:"engine_debug"`
    EngineLogFormat       string              `mapstructure:"engine_log_format"`
    EngineRegistryMirrors map[string][]string `mapstructure:"engine_registry_mirrors"`
```

(`domain` stays stdlib-only.)

**Secret-sourced env extension:** also add to `internal/domain/config.go`
(above `FleetConfig`):

```go
// EnvVarSource selects one key of a Kubernetes Secret as the value of an
// engine container env var (fleet.engine_extra_env_from).
type EnvVarSource struct {
    SecretName string `mapstructure:"secret_name"`
    Key        string `mapstructure:"key"`
}
```

and insert after `EngineExtraEnv` in the new `FleetConfig` fields:

```go
    EngineExtraEnvFrom map[string]EnvVarSource `mapstructure:"engine_extra_env_from"`
```

Map key = env var name on the engine container. Per D7 the type is defined
once in `domain` and reused by the repository — no duplicate
`repository.EnvVarSource`, no conversion in `main.go`.

### Task 2 — `config/loader.go`: viper defaults

After the existing `fleet.*` defaults (line 79) and `log_level` (line 100):

```go
v.SetDefault("fleet.engine_extra_env", map[string]string{})
v.SetDefault("fleet.engine_ca_secret", "")
v.SetDefault("fleet.engine_ca_secret_key", "ca.crt")
v.SetDefault("fleet.engine_debug", false)
v.SetDefault("fleet.engine_log_format", "json")
v.SetDefault("fleet.engine_registry_mirrors", map[string][]string{})
v.SetDefault("fleet.engine_extra_env_from", map[string]any{})
v.SetDefault("log_format", "json")
```

### Task 3 — `internal/observ/logger.go`: configurable format

Replace `NewLogger` (keep `newJSONLogger` → generalize):

```go
// NewLogger builds a structured logrus logger. level falls back to info and
// format falls back to JSON on unrecognized values (per project convention).
// Supported formats: "json", "text".
func NewLogger(level, format string) *logrus.Logger

// NewTestLogger — unchanged behavior (JSON, io.Discard, DebugLevel).
func NewTestLogger() *logrus.Logger
```

Implementation: parse level with existing fallback; when
`strings.EqualFold(format, "text")` use
`&logrus.TextFormatter{TimestampFormat: logTimestampFormat, FullTimestamp: true}`,
otherwise the existing `&logrus.JSONFormatter{TimestampFormat: logTimestampFormat}`.
Refactor `newJSONLogger` into `newLogger(level logrus.Level, format string, out io.Writer) *logrus.Logger`
used by both exported functions.

Update call sites (the only two):
- `cmd/api/main.go:81` and `cmd/api/main.go:359` →
  `logger := observ.NewLogger(cfg.LogLevel, cfg.LogFormat)`

Update the `NewLogger` snippet in `CONTRIBUTING.md` (~line 42) to the new
signature.

### Task 4 — `internal/repository/engine_toml.go` (NEW FILE): TOML rendering

```go
package repository

import (
    "fmt"
    "sort"
    "strings"
)

// engineTOML is the legacy BuildKit-style Dagger engine configuration.
// Engines >= v0.19 automatically read it from /etc/dagger/engine.toml.
type engineTOML struct {
    Debug           bool
    LogFormat       string
    RegistryMirrors map[string][]string
}

// render returns the engine.toml content, or "" when the configuration is
// empty (no debug, no log format, no mirrors). Output is deterministic:
// registry hosts are sorted alphabetically.
func (c engineTOML) render() string

// tomlEscape escapes s for use in a TOML basic ("...") string: backslash,
// double quote, \b \f \n \r \t, and any remaining control byte as \u00XX.
func tomlEscape(s string) string
```

`render()` logic (strings.Builder):
1. If `Debug`: `debug = true\n`.
2. If `LogFormat != ""`: blank separator line when builder non-empty, then
   `[log]\n  format = "<escaped>"\n`.
3. Collect hosts with `len(mirrors) > 0` (hosts with empty mirror lists are
   skipped), `sort.Strings`, then per host: blank separator line when
   builder non-empty, then
   `[registry."<escaped host>"]\n  mirrors = ["<escaped>", "<escaped>"]\n`
   (values joined with `", "`, each wrapped via
   `fmt.Sprintf("\"%s\"", tomlEscape(m))`).
4. Return the builder string ("" when nothing was written).

Example — for `Debug=true`, `LogFormat="json"` and the five mirrors from the
requirement, output is:

```toml
debug = true

[log]
  format = "json"

[registry."docker.elastic.co"]
  mirrors = ["hm-registry.hm.dm.ad/docker-elastic"]
[registry."docker.io"]
  mirrors = ["hm-registry.hm.dm.ad/docker-hub", "mirror.gcr.io"]
[registry."gcr.io"]
  mirrors = ["hm-registry.hm.dm.ad/gcr.io"]
[registry."ghcr.io"]
  mirrors = ["hm-registry.hm.dm.ad/docker-github"]
[registry."public.ecr.aws"]
  mirrors = ["hm-registry.hm.dm.ad/docker-aws"]
```

### Task 5 — `internal/repository/k8s_provider.go`: config, env, volumes

**Constants** (extend existing const block, lines 23–28):

```go
const (
    // ... existing ...
    engineConfigMapName = "dagger-engine-config"
    engineTOMLKey       = "engine.toml"
    engineTOMLPath      = "/etc/dagger/engine.toml"
    engineCAMountPath   = "/etc/ssl/certs/custom-ca.pem"
    volumeDaggerConfig  = "dagger-config"
    volumeCABundle      = "ca-bundle"
)
```

**`K8sProviderConfig`** — append fields (unprefixed style, per file
convention):

```go
type K8sProviderConfig struct {
    // ... existing 14 fields unchanged ...
    ExtraEnv        map[string]string              // literal env vars added to the engine container
    ExtraEnvFrom    map[string]domain.EnvVarSource // env vars sourced from Secret keys (proxy credentials)
    CASecret        string                         // Secret with the custom CA PEM bundle; "" = disabled
    CAKey           string                         // key inside CASecret (default applied in NewK8sProvider)
    Debug           bool                           // engine.toml: debug = true
    LogFormat       string                         // engine.toml: [log] format; "" omits the section
    RegistryMirrors map[string][]string            // engine.toml: [registry."<host>"] mirrors
}
```

**`NewK8sProvider`** — add one default alongside the existing ones:

```go
if cfg.CAKey == "" {
    cfg.CAKey = "ca.crt"
}
```

**Helper** (new method):

```go
// renderEngineTOML renders the fleet-wide Dagger engine configuration.
func (p *K8sProvider) renderEngineTOML() string {
    return engineTOML{
        Debug:           p.cfg.Debug,
        LogFormat:       p.cfg.LogFormat,
        RegistryMirrors: p.cfg.RegistryMirrors,
    }.render()
}
```

**`buildStatefulSet`** changes (lines 126–243):

1. Compute once at the top: `daggerTOML := p.renderEngineTOML()`.
2. Build env dynamically instead of the fixed literal slice (lines 143–153):

```go
env := []corev1.EnvVar{
    {
        Name: "DAGGER_CACHE_TOKEN",
        ValueFrom: &corev1.EnvVarSource{
            SecretKeyRef: &corev1.SecretKeySelector{
                LocalObjectReference: corev1.LocalObjectReference{Name: "engine-registry-auth"},
                Key:                  "token",
            },
        },
    },
}
// Operator-supplied env (proxy vars etc.), sorted for deterministic specs.
extraNames := make([]string, 0, len(p.cfg.ExtraEnv))
for name := range p.cfg.ExtraEnv {
    extraNames = append(extraNames, name)
}
sort.Strings(extraNames)
for _, name := range extraNames {
    env = append(env, corev1.EnvVar{Name: name, Value: p.cfg.ExtraEnv[name]})
}
// Secret-sourced env (proxy credentials etc.), sorted for deterministic specs.
fromNames := make([]string, 0, len(p.cfg.ExtraEnvFrom))
for name := range p.cfg.ExtraEnvFrom {
    fromNames = append(fromNames, name)
}
sort.Strings(fromNames)
for _, name := range fromNames {
    src := p.cfg.ExtraEnvFrom[name]
    env = append(env, corev1.EnvVar{
        Name: name,
        ValueFrom: &corev1.EnvVarSource{
            SecretKeyRef: &corev1.SecretKeySelector{
                LocalObjectReference: corev1.LocalObjectReference{Name: src.SecretName},
                Key:                  src.Key,
            },
        },
    })
}
if p.cfg.CASecret != "" {
    env = append(env,
        corev1.EnvVar{Name: "SSL_CERT_FILE", Value: engineCAMountPath},
        corev1.EnvVar{Name: "NODE_EXTRA_CA_CERTS", Value: engineCAMountPath},
    )
}
```

(`SecretKeyRef` is deliberately NOT marked `Optional`: a missing credential
Secret must keep the pod from starting — see section 4 edge cases.)

3. Volume mounts (replace lines 154–157):

```go
volumeMounts := []corev1.VolumeMount{
    {Name: "dagger-cache", MountPath: "/var/lib/dagger"},
    {Name: "engine-config", MountPath: "/etc/dagger"},
}
if p.cfg.CASecret != "" {
    volumeMounts = append(volumeMounts, corev1.VolumeMount{
        Name:      volumeCABundle,
        MountPath: engineCAMountPath,
        SubPath:   "ca.crt",
        ReadOnly:  true,
    })
}
if daggerTOML != "" {
    volumeMounts = append(volumeMounts, corev1.VolumeMount{
        Name:      volumeDaggerConfig,
        MountPath: engineTOMLPath,
        SubPath:   engineTOMLKey,
        ReadOnly:  true,
    })
}
```

(The subPath file mount coexists with the `/etc/dagger` directory mount.)

4. Pod volumes (replace lines 214–223): keep the existing `engine-config`
   secret volume, then append conditionally:

```go
if p.cfg.CASecret != "" {
    volumes = append(volumes, corev1.Volume{
        Name: volumeCABundle,
        VolumeSource: corev1.VolumeSource{
            Secret: &corev1.SecretVolumeSource{
                SecretName: p.cfg.CASecret,
                Items:      []corev1.KeyToPath{{Key: p.cfg.CAKey, Path: "ca.crt"}},
            },
        },
    })
}
if daggerTOML != "" {
    volumes = append(volumes, corev1.Volume{
        Name: volumeDaggerConfig,
        VolumeSource: corev1.VolumeSource{
            ConfigMap: &corev1.ConfigMapVolumeSource{
                LocalObjectReference: corev1.LocalObjectReference{Name: engineConfigMapName},
            },
        },
    })
}
```

5. Add `"sort"` to imports.

### Task 6 — `internal/repository/k8s_provider.go`: ensure the ConfigMap

In `EnsureStatefulSet` (line 100), before building the STS:

```go
if err := p.ensureEngineConfigMap(ctx); err != nil {
    return fmt.Errorf("ensure engine config map: %w", err)
}
```

New method (mirrors the existing create-or-update pattern of
`EnsureStatefulSet`/`EnsureService`):

```go
// ensureEngineConfigMap creates or updates the fleet-wide ConfigMap holding
// engine.toml. When the rendered config is empty, a stale ConfigMap is
// deleted best-effort so pods are not pinned to a removed volume source.
func (p *K8sProvider) ensureEngineConfigMap(ctx context.Context) error {
    toml := p.renderEngineTOML()
    if toml == "" {
        err := p.clientset.CoreV1().ConfigMaps(p.cfg.Namespace).Delete(ctx, engineConfigMapName, metav1.DeleteOptions{})
        if err != nil && !apierrors.IsNotFound(err) {
            return fmt.Errorf("delete configmap %s: %w", engineConfigMapName, err)
        }
        return nil
    }

    cm := &corev1.ConfigMap{
        ObjectMeta: metav1.ObjectMeta{
            Name:      engineConfigMapName,
            Namespace: p.cfg.Namespace,
            Labels:    map[string]string{engineLabelApp: engineLabelValue},
        },
        Data: map[string]string{engineTOMLKey: toml},
    }
    _, err := p.clientset.CoreV1().ConfigMaps(p.cfg.Namespace).Create(ctx, cm, metav1.CreateOptions{})
    if err != nil && apierrors.IsAlreadyExists(err) {
        existing, getErr := p.clientset.CoreV1().ConfigMaps(p.cfg.Namespace).Get(ctx, engineConfigMapName, metav1.GetOptions{})
        if getErr != nil {
            return fmt.Errorf("get existing configmap %s: %w", engineConfigMapName, getErr)
        }
        cm.ResourceVersion = existing.ResourceVersion
        _, err = p.clientset.CoreV1().ConfigMaps(p.cfg.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
    }
    if err != nil {
        return fmt.Errorf("create/update configmap %s: %w", engineConfigMapName, err)
    }
    return nil
}
```

Notes: the ConfigMap is fleet-wide (shared by all version STSes) and is NOT
deleted by `DeleteStatefulSet` (a version's GC must not remove fleet config).
`EnsureStatefulSet` runs on every `Acquire`, so config edits propagate on the
next acquire without extra reconciliation.

**Secret-sourced env extension:** no changes in this task — listed for
completeness. `engine_extra_env_from` affects only the STS pod template
(Task 5 `buildStatefulSet`); the engine.toml ConfigMap is untouched.

### Task 7 — `cmd/api/main.go`: validation + wiring

Change `createProvider` signature (internal function; sole caller is `run()`
at line 185):

```go
func createProvider(cfg *domain.Config, logger *logrus.Logger) (domain.FleetProvider, error) {
    if err := validateFleetEnv(&cfg.Fleet); err != nil {
        return nil, err
    }
    clientset, err := newK8sClientset()
    if err != nil {
        logger.WithError(err).Warn("failed to create k8s clientset, using stub provider")
        return repository.NewStubProvider(), nil
    }

    k8sCfg := repository.K8sProviderConfig{
        // ... existing 14 field assignments unchanged ...
        ExtraEnv:        cfg.Fleet.EngineExtraEnv,
        ExtraEnvFrom:    cfg.Fleet.EngineExtraEnvFrom, // same domain type; no conversion
        CASecret:        cfg.Fleet.EngineCASecret,
        CAKey:           cfg.Fleet.EngineCASecretKey,
        Debug:           cfg.Fleet.EngineDebug,
        LogFormat:       cfg.Fleet.EngineLogFormat,
        RegistryMirrors: cfg.Fleet.EngineRegistryMirrors,
    }

    return repository.NewK8sProvider(clientset, k8sCfg), nil
}
```

Caller in `run()`:

```go
provider, err := createProvider(cfg, logger)
if err != nil {
    return fmt.Errorf("create fleet provider: %w", err)
}
```

New validation function (same file):

```go
// validateFleetEnv rejects engine env configuration that Kubernetes would
// refuse at StatefulSet admission (duplicate container env names) or that is
// internally inconsistent. Called once at startup (fail fast).
func validateFleetEnv(fleet *domain.FleetConfig) error {
    // DAGGER_CACHE_TOKEN is always injected from a secret; SSL_CERT_FILE and
    // NODE_EXTRA_CA_CERTS are injected when CA injection is enabled.
    reserved := map[string]bool{"DAGGER_CACHE_TOKEN": true}
    if fleet.EngineCASecret != "" {
        reserved["SSL_CERT_FILE"] = true
        reserved["NODE_EXTRA_CA_CERTS"] = true
    }
    for name := range fleet.EngineExtraEnv {
        if name == "" {
            return fmt.Errorf("fleet.engine_extra_env contains an empty env var name")
        }
        if reserved[name] {
            return fmt.Errorf("fleet.engine_extra_env must not set %s: injected by the supervisor", name)
        }
    }
    for name, src := range fleet.EngineExtraEnvFrom {
        if name == "" {
            return fmt.Errorf("fleet.engine_extra_env_from contains an empty env var name")
        }
        if reserved[name] {
            return fmt.Errorf("fleet.engine_extra_env_from must not set %s: injected by the supervisor", name)
        }
        if _, dup := fleet.EngineExtraEnv[name]; dup {
            return fmt.Errorf("env var %s is set in both fleet.engine_extra_env and fleet.engine_extra_env_from", name)
        }
        if src.SecretName == "" {
            return fmt.Errorf("fleet.engine_extra_env_from.%s: secret_name must not be empty", name)
        }
        if src.Key == "" {
            return fmt.Errorf("fleet.engine_extra_env_from.%s: key must not be empty", name)
        }
    }
    if fleet.EngineCASecret != "" && fleet.EngineCASecretKey == "" {
        return fmt.Errorf("fleet.engine_ca_secret_key must not be empty when fleet.engine_ca_secret is set")
    }
    return nil
}
```

### Task 8 — Helm chart

`deploy/helm/dagger-kubernetes/values.yaml` — extend
`supervisor.config.fleet` (after `engineExtraArgs`, line 82):

```yaml
      engineExtraEnv: {}            # extra env vars on engine pods (proxy vars etc.)
      engineExtraEnvFrom: {}        # env vars from Secret keys: {ENV_NAME: {secret_name, key}} (proxy credentials)
      engineCaSecret: ""            # Secret with custom CA PEM bundle; empty = disabled
      engineCaSecretKey: "ca.crt"   # key inside engineCaSecret
      engineDebug: false            # engine.toml debug = true
      engineLogFormat: "json"       # engine.toml [log] format; "" omits
      engineRegistryMirrors: {}     # e.g. {"docker.io": ["mirror.gcr.io"]}
```

and next to `logLevel` (line 89):

```yaml
    logFormat: "json"               # supervisor log format: json | text
```

`deploy/helm/dagger-kubernetes/templates/configmap.yaml` — extend the
`fleet:` block (after `engine_extra_args`, line 75):

```yaml
      engine_extra_env:
{{ toYaml .Values.supervisor.config.fleet.engineExtraEnv | indent 8 }}
      engine_extra_env_from:
{{ toYaml .Values.supervisor.config.fleet.engineExtraEnvFrom | indent 8 }}
      engine_ca_secret: {{ .Values.supervisor.config.fleet.engineCaSecret | quote }}
      engine_ca_secret_key: {{ .Values.supervisor.config.fleet.engineCaSecretKey | quote }}
      engine_debug: {{ .Values.supervisor.config.fleet.engineDebug }}
      engine_log_format: {{ .Values.supervisor.config.fleet.engineLogFormat | quote }}
      engine_registry_mirrors:
{{ toYaml .Values.supervisor.config.fleet.engineRegistryMirrors | indent 8 }}
```

and after `log_level` (line 86):

```yaml
    log_format: {{ .Values.supervisor.config.logFormat | quote }}
```

(`toYaml` on empty maps renders `{}` — valid YAML for viper; a populated
`engineExtraEnvFrom` renders nested `{secret_name, key}` objects that
mapstructure decodes into `map[string]domain.EnvVarSource`.)

### Task 9 — sample config + docs (same changeset, per AGENTS.md)

1. `config/config.app.yaml.sample` — in the fleet "Engine pod template"
   block (after `engine_extra_args`, line 107):

```yaml
  engine_extra_env: {}                            # extra env vars for engine pods, e.g. proxy settings:
    # HTTP_PROXY: "http://proxy.corp.example:3128"
    # HTTPS_PROXY: "http://proxy.corp.example:3128"
    # NO_PROXY: "localhost,127.0.0.1,.svc,.cluster.local"
    # http_proxy: "http://proxy.corp.example:3128"
    # https_proxy: "http://proxy.corp.example:3128"
    # no_proxy: "localhost,127.0.0.1,.svc,.cluster.local"
  engine_extra_env_from: {}                       # env vars sourced from K8s Secret keys (map: env name -> {secret_name, key}).
    # Use for credentials that must never appear in plaintext config, e.g. an authenticated proxy:
    # HTTP_PROXY:
    #   secret_name: "proxy-credentials"
    #   key: "http_proxy"            # secret value: "http://user:pass@proxy.corp.example:3128"
    # HTTPS_PROXY:
    #   secret_name: "proxy-credentials"
    #   key: "https_proxy"
  engine_ca_secret: ""                            # K8s Secret with a custom CA PEM bundle for engine pods; empty = disabled.
  engine_ca_secret_key: "ca.crt"                  # key inside engine_ca_secret holding the PEM bundle.
  engine_debug: false                             # engine.toml: debug = true (verbose engine logging).
  engine_log_format: "json"                       # engine.toml: [log] format; "" omits the section.
  engine_registry_mirrors: {}                     # engine.toml registry mirrors, e.g. {"docker.io": ["mirror.gcr.io"]}.
```

   In the "Logging" section (line 142):

```yaml
log_level: "info"    # debug | info | warn | error
log_format: "json"   # json | text — the supervisor's own log format.
```

2. `config/config.app.yaml` — intentionally UNCHANGED (minimal runtime
   sample; all new keys have sane defaults). Note this in the PR.

3. `docs/README.md`:
   - Config summary table (~lines 297–313): add `fleet` rows for
     `engine_extra_env` (`{}`), `engine_extra_env_from` (`{}`),
     `engine_ca_secret` (`""`),
     `engine_ca_secret_key` (`ca.crt`), `engine_debug` (`false`),
     `engine_log_format` (`json`), `engine_registry_mirrors` (`{}`), and a
     `log_format` row (`json`) next to `log_level`.
   - Env-var section (~line 259): note that map-valued keys
     (`engine_extra_env`, `engine_extra_env_from`,
     `engine_registry_mirrors`, `engine_node_selector`) cannot be overridden
     via `DAGGER_CACHE_` env vars.
   - "Engine fleet" section (~line 355): add subsection
     "### Enterprise engine environment" documenting: proxy env vars via
     `engine_extra_env`; secret-sourced env via `engine_extra_env_from`
     (authenticated-proxy credentials stay in K8s Secrets, never in
     plaintext config or Helm values; referenced Secret/key missing → pod
     fails to start); CA injection (secret requirements, mount path
     `/etc/ssl/certs/custom-ca.pem`, `SSL_CERT_FILE`/`NODE_EXTRA_CA_CERTS`,
     failure mode when the secret/key is missing); generated `engine.toml`
     (ConfigMap `dagger-engine-config`, mounted at `/etc/dagger/engine.toml`,
     read automatically by the engine; config edits apply to new pods).

4. `docs/design/ADR-011-engine-env-ca-config-injection.md` (NEW) — record
   decisions D1–D7 from section 2, alternatives considered (raw TOML string
   passthrough; supervisor-generated Secret instead of ConfigMap; sidecar
   injection; configurable CA mount path; plaintext proxy credentials in
   `engine_extra_env` — rejected in favor of Secret references), and
   consequences (default pod spec
   now includes the engine.toml mount; ConfigMap is fleet-wide). Add the
   row to `docs/design/index.md`.

5. `CONTRIBUTING.md` — update the `NewLogger` snippet (Task 3).

### Task 10 — tests

All table-driven, stdlib `testing` only.

1. `internal/repository/engine_toml_test.go` (NEW) — `TestEngineTOMLRender`:
   - empty config → `""`;
   - debug only; log format only; debug+format;
   - mirrors: hosts sorted alphabetically; multiple mirrors per host;
     hosts with empty mirror lists skipped;
   - exact full-string assertion for the five-registry example from the
     requirements section;
   - escaping: values/hosts containing `"`, `\`, newline, tab, and a control
     byte (`\u0001`) are escaped to valid TOML.
   `TestEngineTOMLEscape` for the escaper edge cases.

2. `internal/repository/k8s_provider_test.go` (extend, using the existing
   `defaultK8sProvider(func(*K8sProviderConfig))` helper):
   - `TestK8sEngineExtraEnv`: `ExtraEnv` with `HTTPS_PROXY`/`https_proxy` →
     both present as literal `Value` envs; `DAGGER_CACHE_TOKEN` still first
     and secret-sourced; env order deterministic (sorted after the token).
   - `TestK8sEngineExtraEnvFrom`: `ExtraEnvFrom` with `HTTP_PROXY` and
     `HTTPS_PROXY` → `{SecretName: "proxy-credentials", Key: "http_proxy"}`
     / `Key: "https_proxy"` → both present on the engine container with
     EMPTY `Value` and `ValueFrom.SecretKeyRef` matching the configured
     secret/key; `DAGGER_CACHE_TOKEN` still first; a combined case with
     `ExtraEnv` + `ExtraEnvFrom` + `CASecret` asserts the full deterministic
     order: token → sorted literal envs → sorted secret-sourced envs → CA
     envs.
   - `TestK8sEngineCAInjection`: `CASecret: "custom-ca-bundle"`,
     `CAKey: "tls-ca.crt"` → volume `ca-bundle` (secret name, Items
     `tls-ca.crt`→`ca.crt`), mount `/etc/ssl/certs/custom-ca.pem` subPath
     `ca.crt` readOnly, envs `SSL_CERT_FILE` and `NODE_EXTRA_CA_CERTS`
     equal to the mount path.
   - `TestK8sEngineCAInjectionDisabled`: default config → no `ca-bundle`
     volume/mount, no `SSL_CERT_FILE` env.
   - `TestK8sEngineTOMLConfigMap`: `Debug: true, LogFormat: "json"` +
     mirrors → after `EnsureStatefulSet`, ConfigMap `dagger-engine-config`
     exists with key `engine.toml` matching the exact expected TOML; STS has
     volume `dagger-config` (ConfigMap source) and mount
     `/etc/dagger/engine.toml` subPath `engine.toml` readOnly.
   - `TestK8sEngineTOMLDefaultLogFormat`: `LogFormat: "json"` only →
     ConfigMap contains exactly `[log]\n  format = "json"\n`.
   - `TestK8sEngineTOMLEmpty`: zero-value TOML config → no ConfigMap
     created, no `dagger-config` volume/mount; pre-create a stale ConfigMap
     and assert it is deleted by `EnsureStatefulSet`.
   - `TestK8sEngineConfigMapIdempotent`: `EnsureStatefulSet` twice with the
     same config → no error (update path), ConfigMap content unchanged.
   - Existing `TestK8sEngineEnvironVariables` keeps passing unchanged.

3. `config/loader_test.go`:
   - `TestLoadDefaults`: assert `cfg.LogFormat == "json"`,
     `cfg.Fleet.EngineLogFormat == "json"`,
     `cfg.Fleet.EngineCASecretKey == "ca.crt"`,
     `cfg.Fleet.EngineCASecret == ""`, `cfg.Fleet.EngineDebug == false`,
     and the three new maps (`EngineExtraEnv`, `EngineRegistryMirrors`,
     `EngineExtraEnvFrom`) non-nil/empty.
   - `TestLoadFile`: extend the YAML fixture with
     `log_format: "text"`, `fleet.engine_extra_env` (two proxy vars),
     `fleet.engine_extra_env_from` (`HTTP_PROXY` → secret
     `proxy-credentials`, key `http_proxy`),
     `fleet.engine_registry_mirrors` (`docker.io` with two mirrors),
     `fleet.engine_ca_secret`, `fleet.engine_debug: true` — assert parsed
     values (proves `map[string][]string` and
     `map[string]domain.EnvVarSource` mapstructure wiring).

4. `internal/observ/observ_test.go`:
   - update existing `NewLogger(level)` calls to `NewLogger(level, "json")`;
   - `TestNewLoggerFormat`: `"text"` → `*logrus.TextFormatter`; `"json"`,
     `""`, `"bogus"` → `*logrus.JSONFormatter` (type assertions); both keep
     the level fallback behavior.

5. `cmd/api/main_test.go` — `TestValidateFleetEnv` table-driven:
   - valid proxy map → nil;
   - `DAGGER_CACHE_TOKEN` in map → error;
   - empty env name → error;
   - `SSL_CERT_FILE` in map WITH `EngineCASecret` set → error;
   - `SSL_CERT_FILE` in map WITHOUT CA secret → nil;
   - `EngineCASecret` set + `EngineCASecretKey == ""` → error;
   - valid `EngineExtraEnvFrom` entry (secret + key) → nil;
   - reserved name (`DAGGER_CACHE_TOKEN`) in `EngineExtraEnvFrom` → error;
   - same name in BOTH `EngineExtraEnv` and `EngineExtraEnvFrom` → error;
   - `EngineExtraEnvFrom` entry with empty `SecretName` → error;
   - `EngineExtraEnvFrom` entry with empty `Key` → error.

6. Optional (build-tagged, real cluster): extend
   `internal/repository/k8s_provider_integration_test.go` with a case that
   sets the new config fields and asserts ConfigMap + volumes on the real
   STS; add cleanup of the ConfigMap to `cleanupProvider`.

Coverage: every new branch (CA on/off, ExtraEnvFrom empty/populated, TOML
empty/non-empty, escaper paths, format fallback, all validation errors) is
exercised — maintains the 100% target for touched packages.

---

## 4. Edge cases & error handling (summary)

| Case | Behavior |
|------|----------|
| `engine_extra_env` empty | No extra envs; pod spec unchanged apart from engine.toml mount. |
| `engine_extra_env` contains reserved name (`DAGGER_CACHE_TOKEN`, or `SSL_CERT_FILE`/`NODE_EXTRA_CA_CERTS` when CA enabled) | Supervisor fails at startup with an actionable error (before any K8s call). |
| `engine_extra_env` empty key | Startup error. |
| `engine_extra_env_from` empty | No secret-sourced envs; pod spec unchanged. |
| `engine_extra_env_from` has reserved name, name duplicated with `engine_extra_env`, empty name, or empty `secret_name`/`key` | Supervisor fails at startup with an actionable error (same fail-fast validation as `engine_extra_env`). |
| `engine_extra_env_from` references a Secret or key missing in cluster | Pod fails to start (`CreateContainerConfigError`) until the operator fixes the Secret (standard K8s behavior; deliberate — NOT `Optional`: without proxy credentials the engine cannot reach the network anyway). Documented. |
| `engine_ca_secret` empty | No CA volume/mount/envs (pre-change behavior). `engine_ca_secret_key` is ignored. |
| `engine_ca_secret` set but Secret/key missing in cluster | Pod stays `CreateContainerConfigError` until the operator fixes the Secret (standard K8s behavior; deliberate — NOT `Optional`, so we never run with a dangling `SSL_CERT_FILE`). Documented. |
| Rendered TOML empty | No ConfigMap volume/mount; stale ConfigMap deleted best-effort. |
| Mirror host with empty mirror list | Host skipped in TOML. |
| TOML special characters in hosts/mirrors/log format | Escaped (`tomlEscape`); output always valid TOML. |
| Config changed at runtime | Next `EnsureStatefulSet` (every acquire) updates ConfigMap + STS template; already-running pods keep old config until restarted/scaled. Documented. |
| `DAGGER_CACHE_` env override for map keys | Not supported by viper for maps — documented limitation (same as `engine_node_selector` today). |
| Stub provider path (no kubeconfig) | Unchanged; validation still runs first, so bad config fails even in stub mode. |

## 5. Rollout / backward compatibility

- No `domain.FleetProvider` interface change → stub provider,
  `internal/handler` tests, `tests/integration` unaffected.
- No RBAC changes (configmaps already permitted; SecretKeyRef envs are
  resolved by the kubelet, not the supervisor's ServiceAccount).
- `fleet.engine_extra_env_from` defaults to empty → zero behavior change for
  existing deployments. Migration path off plaintext proxy credentials:
  create the Secret, move the entry from `engine_extra_env` to
  `engine_extra_env_from`, roll engine pods.
- Default behavior change (intended, per requirements): engine pods get a
  `dagger-config` volume with `engine.toml` containing `[log] format = "json"`.
  Existing STSes are updated in place on their next acquire (rolling template
  update); running pods pick it up on restart.
- Operators opting out entirely: set `engine_log_format: ""` (and nothing
  else) → pod spec reverts to pre-change shape.

## 6. Validation steps for the implementer

1. `gofmt -l . && goimports -l -local github.com/disaster/dagger-kubernetes .`
2. `go vet ./...`
3. `go build ./...`
4. `go test ./...` (unit) and `go test -cover ./internal/repository/... ./config/... ./internal/observ/... ./cmd/api/...`
5. `helm template deploy/helm/dagger-kubernetes` — confirm the rendered
   ConfigMap YAML is valid and new keys appear under `fleet:`.
6. Optional: `go test -tags integration ./internal/repository/...` against a
   kind cluster (needs `KUBECONFIG`).

## 7. Out of scope

- `engine.json` (modern Dagger config format) — TOML covers the requirements
  and some TOML-exclusive options.
- Per-registry `ca`/`insecure` flags in engine.toml, engine GC/log-level
  settings — not requested; `engineTOML` struct is extensible later.
- Configurable CA mount path (fixed `/etc/ssl/certs/custom-ca.pem`; extra
  env-based pointers can be added via `engine_extra_env`).
- `deploy/k8s/namespace-rbac.yaml` static quickstart ConfigMap (already
  intentionally partial; Helm is the recommended deployment).
- Stub provider / `internal/handler/test_helper_test.go` changes (none
  needed — no pod rendering in the stub).
- Non-Secret env sources for `engine_extra_env_from` (ConfigMapKeyRef,
  FieldRef) and whole-secret `envFrom` injection — per-variable SecretKeyRef
  only. `domain.EnvVarSource` can grow fields later if needed.

# ADR-011: Engine proxy, CA, and engine.toml config injection

- **Status:** Accepted
- **Date:** 2026-08-10
- **Supersedes:** none
- **Related:** ADR-004 (per-version StatefulSet autoscaler), ADR-009 (clean architecture layering)

## Context

Enterprise operators needed to run Dagger engines behind a corporate HTTP
proxy, trust a private CA, and tune the engine's own logging/registry-mirror
configuration. The supervisor's engine pod template had no hooks for any of
these: env vars were limited to the supervisor-injected `DAGGER_KUBERNETES_TOKEN`,
there was no CA injection, and the engine's `engine.toml` was never mounted.

A related concern: authenticated proxies require credentials inside
`HTTP_PROXY`/`HTTPS_PROXY`. Storing those credentials in plaintext config
(Helm values, the config file, git) is unacceptable.

## Decision

Add four independent, composable knobs to `fleet.*` config, all validated
at supervisor startup and rendered into the engine StatefulSet pod template:

1. **D1 — Proxy env vars** (`fleet.engine_extra_env`,
   `map[string]string`): injected as literal env vars, sorted by name.
   Mirrors the existing `engine_extra_args` naming. Covers
   `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` and any future env.
2. **D2 — CA bundle** (`fleet.engine_ca_secret` + `engine_ca_secret_key`,
   default `ca.crt`): references an existing K8s Secret. Mounted read-only
   at fixed path `/etc/ssl/certs/custom-ca.pem` (Secret key normalized to
   file `ca.crt` via volume `Items`); `SSL_CERT_FILE` and
   `NODE_EXTRA_CA_CERTS` point at it. NOT `Optional` — a missing Secret/key
   fails the pod loudly.
3. **D3 — Generated `engine.toml`** (`fleet.engine_debug`,
   `fleet.engine_log_format`, `fleet.engine_registry_mirrors`): structured,
   validated fields. The K8s provider renders TOML by hand (`fmt.Sprintf`
   + a small escaper — no new dependency) and stores it in a fleet-wide
   ConfigMap `dagger-engine-config` (key `engine.toml`), ensured on every
   `EnsureStatefulSet`, mounted via `subPath` at
   `/etc/dagger/engine.toml` (engines v0.19+ read this path automatically).
   When the rendered TOML is empty, no volume/mount is added and a stale
   ConfigMap is deleted best-effort.
4. **D4 — Supervisor log format** (top-level `log_format`, default
   `json`): `observ.NewLogger(level, format)` supports `json`/`text`.
   Supervisor-wide concern, separate from the engine's own `[log] format`
   in `engine.toml`.
5. **D7 — Secret-sourced env vars** (`fleet.engine_extra_env_from`,
   `map[string]domain.EnvVarSource` with fields `secretName`, `key`):
   each entry is injected as `corev1.EnvVar{Name, ValueFrom.SecretKeyRef}`
   on the engine container, sorted by name, after the literal
   `engine_extra_env` entries. `SecretKeyRef` is NOT `Optional`. The
   `EnvVarSource` type lives in `domain` (stdlib-only per the dependency
   rule) and is reused as-is by `K8sProviderConfig.ExtraEnvFrom` — no
   duplicate `repository.EnvVarSource`, no conversion in `main.go`.
   (The field was originally `secret_name`; renamed to `secretName` to
   match the camelCase convention of every other key rendered by the Helm
   chart into the app config.)
6. **D6 — Validation location**: `createProvider` (package main) gains an
   `error` return and calls `validateFleetEnv` before provider
   construction. Duplicate container env names (which K8s would reject at
   STS admission) and cross-map duplicates are caught at supervisor
   startup with an actionable error.

### Consequences

- The default pod spec now includes the `dagger-config` volume with
  `engine.toml` containing `[log] format = "json"` (the default
  `engine_log_format`). Existing STSes are updated in place on their next
  acquire; running pods pick it up on restart. Operators opting out
  entirely set `engine_log_format: ""` (and nothing else) to revert to the
  pre-change pod spec.
- The ConfigMap is fleet-wide (shared by all version STSes) and is NOT
  deleted by `DeleteStatefulSet` — a version's GC must not remove fleet
  config.
- No `domain.FleetProvider` interface change → stub provider,
  `internal/handler` tests, and `tests/integration` are unaffected.
- No RBAC changes: `configmaps` CRUD was already permitted, and
  `SecretKeyRef` envs are resolved by the kubelet, not the supervisor's
  ServiceAccount.

## Alternatives considered

- **Raw TOML string passthrough** (`fleet.engine_toml: "..."`) — rejected:
  it would defeat validation and deterministic rendering, and push TOML
  syntax errors into the supervisor's startup path. Structured fields keep
  the type safety and let the supervisor render valid TOML.
- **Supervisor-generated Secret instead of ConfigMap** for `engine.toml`
  — rejected: `engine.toml` is not secret material, and ConfigMaps are the
  K8s-native way to ship non-sensitive config. Using a Secret would add
  RBAC surface and operator confusion.
- **Sidecar injection** (an init container that writes `engine.toml`) —
  rejected: a ConfigMap + `subPath` mount is simpler, has no image
  dependency, and is what the upstream Dagger docs recommend.
- **Configurable CA mount path** — rejected: a fixed well-known path
  (`/etc/ssl/certs/custom-ca.pem`) keeps the `SSL_CERT_FILE`/`NODE_EXTRA_CA_CERTS`
  env vars predictable. The escape hatch is `engine_extra_env` for
  additional env-based pointers.
- **Plaintext proxy credentials in `engine_extra_env`** — rejected in
  favor of Secret references (`engine_extra_env_from`): credentials in
  Helm values / config files / git are unacceptable for authenticated
  proxies. K8s `SecretKeyRef` is the native mechanism and needs no extra
  RBAC (the kubelet resolves refs, not the supervisor SA).
- **`envFrom` / whole-secret injection, ConfigMap/FieldRef sources** —
  out of scope: per-variable `SecretKeyRef` covers the authenticated-proxy
  use case. `domain.EnvVarSource` can grow fields later if needed.

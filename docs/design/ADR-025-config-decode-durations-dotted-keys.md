# ADR-025: Robust config decoding — extended durations and dot-safe map keys

- **Status:** accepted
- **Date:** 2026-08-25
- **Deciders:** dagger-kubernetes maintainers

## Context

Two classes of real-world config values crashed the supervisor at startup
(`config.Load`), both caused by Viper's default unmarshal path:

1. **Duration values using day/week units.** `auth.jwt.refresh_ttl: "7d"` and
   `history.gc.max_age: "7d"` failed with
   `time: unknown unit`: Viper's default decode hook chains
   `mapstructure.StringToTimeDurationHookFunc`, which calls
   `time.ParseDuration`, and Go's parser has no `d` (day) or `w` (week) unit.
   Users naturally write `7d`; the failure mode is a crash-loop with an
   inscrutable error.

2. **Map keys containing dots.** Viper's `Unmarshal` rebuilds the settings tree
   via `viper.getSettings`, which splits **every** flat key on `.` and re-nests
   it. A map key that itself contains a dot is therefore corrupted into nested
   maps: `fleet.engine_registry_mirrors: {"docker.io": [...]}` decoded as
   `{"docker": {"io": ...}}` and failed with
   `'fleet.engine_registry_mirrors[docker][0]' expected type 'string', got
   unconvertible type 'map[string]interface {}'`. The same corruption hit
   Longhorn PVC label keys (`recurring-job-group.longhorn.io/nobackup`) in
   `fleet.engine_pvc_labels`, node-selector keys, and dotted env names in
   `fleet.engine_extra_env_from`.

## Decision

`config.Load` no longer calls `viper.Unmarshal`. It keeps Viper for everything
else (defaults via `SetDefault`, env prefix/replacer, `AutomaticEnv`,
`ReadInConfig`) and replaces only the final decode step
(`unmarshalConfig` in `config/loader.go`):

### 1. Structure-preserving settings tree (`collectSettings`)

The merged settings are rebuilt leaf-by-leaf: for each key reported by
`viper.AllKeys`, the value is resolved with `v.Get` (which applies the full
override/flags/env/config/defaults priority and already resolves dotted map
keys via Viper's prefix search on the config source), then inserted by walking
the **real** nested structure (`insertSetting`). At each level the walk probes
the source subtree for the longest remaining prefix that is an actual map key
(`exactKeyPrefix`), so dotted keys stay intact. When no structural hint exists
(e.g. env-only leaves, Viper `Set` override trees), the walk falls back to
consuming one path element — the same nesting Viper would produce for
dot-free keys. This preserves the exact same leaf-priority semantics
(defaults < file < env, per-leaf resolution, env shadowing) as the previous
`viper.Unmarshal` path.

### 2. Extended duration parsing (`parseExtendedDuration`)

Decoding uses `go-viper/mapstructure/v2` directly (already a transitive
dependency of Viper) with the same weak-typing configuration Viper uses —
`WeaklyTypedInput: true` plus the weak string→slice hook — but with the
duration hook replaced by `stringToDurationHookFunc`, which parses with
`parseExtendedDuration`: standard `time.ParseDuration` grammar **plus** `d`
(day) and `w` (week) units, including fractional (`1.5d`), negative (`-7d`)
and composite (`2d3h`) forms. Inputs that fail both grammars keep
`time.ParseDuration`'s error text unchanged, and decoding still fails fast
with the offending key path.

## Alternatives considered

- **Fix the values instead (reject `7d`, quote/dodge dotted keys).** Works for
  one deployment but leaves a startup crash waiting for the next user;
  dotted keys (registry hostnames, K8s label keys) are inherent to the domain
  and cannot be dodged.
- **Use `viper.UnmarshalKey` per top-level subtree.** `v.Get` returns subtrees
  with keys intact, but it does not merge across sources at leaf level, so a
  partially-specified file would silently lose all defaults of that section.
- **Parse the YAML file directly and reimplement layering.** Fully sidesteps
  Viper's tree handling but duplicates env-prefix, shadowing and priority
  semantics — more code, more drift risk. The chosen approach keeps Viper as
  the single source of truth for resolution and only fixes tree assembly.
- **Post-process Viper's decode errors.** Not possible: mapstructure fails
  hard on the corrupted nested maps before a partial result is available.

## Consequences

- `"7d"`, `"1w"`, `"1.5d12h"`-style durations work in the config file and via
  `DAGGER_KUBERNETES_` env overrides; invalid values still fail fast with a
  key-path-annotated error.
- Map fields with dotted keys (`fleet.engine_registry_mirrors`,
  `fleet.engine_pvc_labels`, `fleet.engine_node_selector`,
  `fleet.engine_extra_env`, `fleet.engine_extra_env_from`) decode verbatim.
- `config` package keeps 100% statement coverage; `go-viper/mapstructure/v2`
  is promoted to a direct dependency (same version Viper already used).
- Env-var behavior is unchanged: map-valued keys remain non-bindable via env
  (same as before), and leaf keys keep the documented `DAGGER_KUBERNETES_`
  mapping.

# ADR-043: Engine (fleet runner) image pulled through the cache mirror

**Status:** Accepted  
**Date:** 2026-09-25

## Context

[ADR-033](ADR-033-local-image-mirror.md) built the local image cache — one
Zot on-demand pull-through mirror per upstream — and wired it into the
engine's `engine.toml` registry mirrors so **pipeline** pulls (`container
from`, `with-exec`, …) hit the in-cluster cache. Its Scope bullet explicitly
left a gap: *"The engine image itself is pulled by the kubelet before the pod
starts and is not routed through the mirror — that remains
`fleet.engine_image_registry` + `engine-image-auth`."*

Issue #40 ("cache image of runners") asks to close that gap: when the image
cache is enabled **and** the `registry.dagger.io` upstream preset is enabled,
deploying a fleet runner should use the cache for the engine image too.
Today every engine StatefulSet pulls `registry.dagger.io/engine:<version>`
directly from the internet — per node, per rollout, consuming upstream
bandwidth and risking rate limits, even though another node may have just
pulled the same layers.

Two constraints shape the design:

1. **The engine image is a kubelet pull.** `engine.toml` (and BuildKit's
   `http = true` mirror marking) only governs pipeline image pulls inside the
   engine. The kubelet resolves the StatefulSet's container `image` itself,
   before any engine config exists.
2. **The kubelet will not dial a plaintext in-cluster registry** unless every
   node's container runtime is configured with an insecure-registry
   allowlist. That is node-scoped configuration the chart cannot render, and
   it weakens transport security.

## Decision

### D1 — Automatic, data-driven rewrite of the engine image reference

The rewrite is computed at deploy time, not configured by a new flag. The
supervisor already receives the chart-rendered `image_cache.mirrors[]` block;
`K8sProvider.GetEngineImage(version)` passes `fleet.engine_image_registry`
through a pure domain helper before appending the version tag:

```go
// internal/domain/engine_image.go
func EngineImageRegistryViaMirror(registry string, mirrors []ImageCacheMirror) string
```

- A mirror matches when its `Host` equals the host portion of the registry
  reference (`registry.dagger.io/engine` → host `registry.dagger.io`) **and**
  the mirror is marked `tls: true` **and** has a non-empty `InternalAddr`.
- On a match, the mirror's `InternalAddr` replaces the host and the repository
  path is preserved:
  `registry.dagger.io/engine:v0.21.4` →
  `<release>-registry-dagger-io-mirror.<namespace>.svc:5000/engine:v0.21.4`.
- Scheme-prefixed (`https://…`) or otherwise malformed references never
  rewrite (defensive `splitRegistryRef` guard); no matching mirror means the
  registry is returned unchanged.

**The rewrite is gated on `tls: true`.** A plaintext mirror (today's
default) never triggers it, so existing deployments cannot hit a silent
`ImagePullBackOff` on clusters without insecure-registry configuration.
With no `registry.dagger.io` mirror configured, behavior is exactly as
before — the gate is data (the mirror list), not a flag.

### D2 — TLS on the mirrors, with a CA-trust surface

`imageCache.tls.enabled` + `imageCache.tls.secretName` (operator-supplied
Secret with `tls.crt` + `tls.key` valid for every mirror hostname; a wildcard
`*.<namespace>.svc` cert covers all mirrors) makes Zot serve HTTPS:
`http.tls.cert`/`key` render into `config.json`, the cert/key Secret mounts at
`/etc/zot/tls`, and the mirror probes switch to `scheme: HTTPS`. Enabling TLS
without a `secretName` fails `helm template` (`required`).

CA trust has two distinct surfaces:

- **Supervisor (admin image-cache client):** `imageCache.tls.caSecretName`
  (key `imageCache.tls.caSecretKey`, default `ca.crt`) is mounted into the
  supervisor StatefulSet at
  `/etc/dagger-kubernetes/image-cache-ca/<key>`, and
  `image_cache.tls_ca_path` points at it. The client is dialed over `https://`
  with that pool (`repository.NewDistributionClientForMirror` /
  `LoadCertPool`); an unreadable/unparsable bundle fails startup
  (`load image cache TLS CA: …`), consistent with the fail-fast wiring
  elsewhere. Empty path = system trust pool.
- **Kubelet (engine-image pull):** out of scope — see Consequences. The
  chart cannot render containerd/CRI-O `certs.d`/`hosts.toml`; trusting the
  mirror CA on every node remains an operator prerequisite.

### D3 — `engine_registry_mirrors_http` must drop TLS mirrors

`fleet.engine_registry_mirrors_http` makes `engine.toml` emit
`[registry."<mirror>"] http = true` per entry (ADR-033 D4). When the mirrors
serve HTTPS, listing them there would make BuildKit dial plaintext HTTP
against a TLS listener and **every pipeline pull would fail**. The chart's
`imageCacheMirrorHosts` helper therefore returns an empty list when
`imageCache.tls.enabled` (while `engineRegistryMirrors` keeps merging the
mirror addresses themselves — BuildKit then dials them over HTTPS, which
requires the node/BuildKit trust for the mirror CA as well). Marking stays
explicit: plaintext mirrors keep `http = true` exactly as today.

## Alternatives considered

- **A supervisor flag to force engine-image routing** — rejected: the mirror
  list already carries everything needed (host match + `tls`); a flag would
  duplicate that state and could contradict it (flag on with no TLS mirror =
  broken pulls).
- **Plaintext mirror + node-level insecure-registry allowlist** — rejected:
  node-scoped config the chart cannot render, and it disables registry TLS
  verification on every node. TLS-on-mirror turns the prerequisite into
  "trust this CA" instead of "allow insecure".
- **Only cache pipeline pulls (status quo)** — rejected: that is the gap the
  issue names; fleet-wide engine pulls are the most repetitive large pull.
- **Force `imagePullPolicy: Always` on the rewritten image** — not needed: a
  rewritten reference is a different image ref, so the kubelet pulls it from
  the mirror on first use; `fleet.engine_pull_policy` (`IfNotPresent`)
  stays untouched.
- **Rewrite in the Helm chart (render the StatefulSet image)** — rejected:
  StatefulSets are rendered by the supervisor at acquire time, not by Helm;
  the rewrite belongs where `GetEngineImage` already builds the reference.

## Consequences

- Config: `image_cache.mirrors[].tls` (bool) and `image_cache.tls_ca_path`
  (string, default `""`) in the supervisor config; new Helm block
  `imageCache.tls.{enabled,secretName,caSecretName,caSecretKey}` (all off by
  default). Chart-rendered `configmap.yaml`, `statefulset.yaml` (CA mount),
  `image-cache.yaml` (Zot TLS), `_helpers.tpl` (`imageCacheMirrorHosts`).
- A bad mirror CA path fails supervisor startup instead of degrading to
  unverified TLS or plaintext.
- **Operator prerequisites:** the kubelet on engine nodes must trust the
  mirror CA (containerd/CRI-O `certs.d`/`hosts.toml`); the `secretName`
  cert must cover every mirror Service hostname. Neither is automated.
- **Degraded mode:** a mirror that is down at acquire/pull time yields
  `ImagePullBackOff` for the engine pod (the supervisor does not orchestrate
  pulls and never falls back to the upstream) — the same property pipelines
  already have for configured mirrors (ADR-033).
- **Digest pinning / content trust** of engine images is unchanged: pulls
  remain by tag (`:<version>`), and only the **engine** image is routed —
  the supervisor's own image and the Zot/MinIO images are not.
- Follow-ups: cert-manager auto-issuance of the mirror
  `Certificate` (reusing the chart's `data-cert.yaml` pattern) and
  private-upstream credentials (ADR-033) remain open.

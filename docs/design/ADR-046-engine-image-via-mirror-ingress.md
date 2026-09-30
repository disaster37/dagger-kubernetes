# ADR-046: Engine image routed through a TLS-terminated mirror ingress

**Status:** Accepted  
**Date:** 2026-09-30

## Context

[ADR-043](ADR-043-engine-image-via-cache.md) closed the gap left by
[ADR-033](ADR-033-local-image-mirror.md): when a mirror's `host` matches the
host of `fleet.engine_image_registry`, the supervisor rewrites the engine
StatefulSet image to pull `registry.dagger.io/engine:<version>` through the
local Zot mirror — but only when the mirror is marked `tls: true`
(`imageCache.tls.enabled`, default `false`) and the `registry.dagger.io`
preset is enabled (also default `false`). In practice neither is on by
default, so enabling the image cache alone never triggered the rewrite and the
engine kept hitting the public registry (issue #55).

Even when it fired, the rewrite emitted the mirror's `internal_addr` — a
cluster-internal `<release>-registry-dagger-io-mirror.<namespace>.svc:5000`
FQDN. The kubelet runs on the node and cannot resolve cluster `.svc` names, so
the live environment needed a manual `/etc/hosts` pin plus containerd
`certs.d`/`hosts.toml` CA configuration on every node to make the pull work —
functional, but not "automatic".

## Decision

### D1 — A node-reachable ingress address (`external_addr`) drives the rewrite

1. `image_cache.mirrors[]` gains an optional `external_addr` field: the
   node-reachable hostname of a dedicated mirror ingress. It is
   chart-rendered (no new supervisor validation).
2. The chart renders that ingress for the `registry.dagger.io` mirror from
   the new `imageCache.registryIngress` block
   (`enabled`, `host`, `className`, `annotations`, `tls.secretName`), gated on
   `imageCache.enabled` + the `registry.dagger.io` preset +
   `imageCache.registryIngress.enabled`. The ingress terminates TLS in front
   of the mirror Service (port 5000), adding
   `nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"` only when
   `imageCache.tls.enabled` (the mirror itself may stay plaintext).
3. `EngineImageRegistryViaMirror` (the ADR-043 D1 helper) now selects the
   address in order:
   1. **`ExternalAddr`** when non-empty — the ingress host; the `tls` gate no
      longer applies, because the ingress (not the mirror) presents TLS to the
      kubelet.
   2. **`TLS && InternalAddr`** — the legacy ADR-043 in-cluster path, kept as a
      fallback for deployments that wired node DNS + CA trust manually.
   3. Otherwise the registry is returned unchanged (a plaintext `internal_addr`
      alone still never rewrites).

**Supersession:** ADR-043's D1 statement *"the rewrite is gated on `tls: true`"*
is superseded by this ADR. ADR-043's D2 (TLS on the mirrors) and D3
(`engine_registry_mirrors_http` dropping TLS mirrors) are unchanged;
`internal_addr` keeps serving BuildKit/`engine.toml` pipeline-mirror traffic
([ADR-033](ADR-033-local-image-mirror.md)).

The kubelet then dials `<registryIngress.host>:443` (public/cert-manager
certificate) → ingress → mirror Service → Zot pull-through fetches
`registry.dagger.io/engine:<version>`. The supervisor's rewrite stays a pure,
stateless function; no new logging or concurrency.

## Alternatives considered

- **Keep the `tls` gate and document enabling `imageCache.tls.enabled`** —
  rejected: it still emits a `.svc` address the kubelet cannot resolve, so the
  node-level `/etc/hosts` + CA prerequisites remain; the issue asks for
  automatic routing.
- **Rewrite to a node-local registry mirror (containerd `registry.mirrors`
  config)** — rejected: node-scoped container-runtime configuration the chart
  cannot render (same class of objection as ADR-043's insecure-registry
  alternative).
- **Run the mirror ingress for every preset** — rejected (scope): only the
  engine image (`registry.dagger.io`) needs a kubelet-reachable endpoint;
  pipeline pulls stay in-cluster via `engine.toml`.
- **Supervisor flag to force engine-image routing** — remains rejected for the
  same reasons as ADR-043: the mirror list already carries the data.

## Consequences

- Config: `image_cache.mirrors[].external_addr` (optional string). Chart: new
  `imageCache.registryIngress.{enabled,host,className,annotations,tls.secretName}`
  values (all off/generic by default), a `configmap.yaml` `external_addr`
  render for the `registry.dagger.io` mirror, and the
  `templates/image-cache-registry-ingress.yaml` Ingress.
- Enabling `imageCache.enabled` alone still does not rewrite (the preset and
  ingress must be on) — behavior otherwise backwards compatible: the legacy
  `tls`+`internal_addr` rewrite keeps working, and plaintext `internal_addr`
  alone still never rewrites.
- Operator prerequisites (documented, not automated): the ingress hostname
  must resolve from the engine nodes, and `registryIngress.tls.secretName`
  should hold a cert the kubelet trusts — an empty `secretName` makes the
  ingress serve plaintext and the pull fails without insecure-registry node
  config.
- A mirror that is down at pull time still yields `ImagePullBackOff`; the
  supervisor does not orchestrate pulls (unchanged, ADR-033/ADR-043).
- Follow-up unchanged from ADR-043: cert-manager auto-issuance and
  private-upstream credentials remain open.

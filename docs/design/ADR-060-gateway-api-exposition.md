# ADR-060: Gateway API exposition support (NGINX Gateway Fabric)

**Status:** Accepted  
**Date:** 2026-11-25

## Context

The chart has always exposed the platform through `networking.k8s.io/v1`
`Ingress` resources: a control-plane `Ingress` (UI + API + OTLP + the
`/v1/engine` WebSocket), a data-plane `Ingress` with
`nginx.ingress.kubernetes.io/ssl-passthrough: "true"` (or an F5
`TransportServer`), and a mirror `Ingress` for the Zot pull-through cache
([ADR-046](ADR-046-engine-image-via-mirror-ingress.md)).

Two forces pushed us off that model:

1. **The F5 `TransportServer` HTTP-01 deadlock.** In `transportServer` mode no
   data-plane `Ingress` renders, so cert-manager's HTTP-01 solver has no
   `Ingress` to attach its `cm-acme-http-solver-*` challenge to for the data
   host — renewal stalls and the operator must fall back to `dataCert.enabled`.
   The community `ingress-nginx` controller (which the `annotation` passthrough
   mode targets) is **end-of-life**.
2. **Gateway API is the successor.** `Gateway` / `HTTPRoute` / `TLSRoute` /
   `BackendTLSPolicy` are GA (`gateway.networking.k8s.io/v1`) and NGINX Gateway
   Fabric (NGF) is a supported, actively maintained implementation.

## Decision

### D1 — A dedicated `gateway.*` values block, off by default

A new `## @section Gateway API` block mirrors the existing exposition knobs
(`gateway.enabled`, `gateway.create`, `gateway.control.*`, `gateway.data.*`,
`gateway.registryMirror.*`, `gateway.dex.*`). `gateway.enabled` defaults to
`false`, so the default render is byte-for-byte unchanged. Ingress and Gateway
may render **simultaneously** (disjoint API groups) to support staged
migrations.

**URL precedence.** `dagger-kubernetes.publicUrl` prefers
`gateway.enabled && gateway.control.enabled` over `ingress.enabled`;
`dagger-kubernetes.dataHostname` prefers `gateway.enabled && gateway.data.enabled`
over `dataIngress.enabled`. `image_cache.mirrors[].external_addr` follows
`gateway.registryMirror.host` when the mirror route is on, else
`imageCache.registryIngress.host`. The `dataCert` `dnsNames[0]` follows
`gateway.data.host` when gateway + data are on, else `dataIngress.host`. The
data-plane TLS Secret (`dagger-kubernetes.dataTLSSecretName`) resolves
`gateway.data.tls.secretName` → `dataIngress.tls.secretName` → `""`.

### D2 — One Gateway mixing HTTP, HTTPS Terminate, and TLS Passthrough

When `gateway.create=true` the chart renders a single `Gateway` with:

- an `http` listener on `:80` with **no `hostname`** — so cert-manager's ACME
  `gatewayHTTPRoute` solver can attach a challenge `HTTPRoute` for **any** host
  (including the data host), resolving the ADR-046 deadlock;
- `:443` HTTPS `Terminate` listeners (`https`, `https-mirror`, `https-dex`) for
  the control, mirror, and Dex hosts, with `certificateRefs` defaulting to
  `<fullname>-gateway-tls` / `-mirror-tls` / `-dex-tls`;
- a `:443` TLS `Passthrough` listener (`tls-data`) restricted to `TLSRoute`
  kinds for the mTLS data plane.

The chart fails closed when two `:443` listeners would bind the same hostname.
`gateway.create=false` attaches the routes to an existing shared Gateway via
`gateway.parentRef` (cross-namespace supported) — the escape hatch for
platform teams that own one Gateway per cluster.

### D3 — `BackendTLSPolicy` bridges the HTTPS upstreams

The supervisor `:8080` listener serves HTTPS, so the control `HTTPRoute` carries
a `BackendTLSPolicy` verifying `<fullname>-control.<namespace>.svc` against the
minting-CA Secret (`<fullname>-minting-ca`, [ADR-005](ADR-005-embedded-minting-ca.md)).
The mirror route carries one when `imageCache.tls.enabled`, verifying against
`imageCache.tls.caSecretName` (or `wellKnownCACertificates: System` when the
mirror cert is publicly trusted). `caCertificateRefs` only read the `ca.crt`
key, so the chart fails closed when `imageCache.tls.caSecretKey != "ca.crt"`.

### D4 — Toggleable NGF v1alpha1 policies

NGF-specific `gateway.nginx.org/v1alpha1` attachments are rendered only when
`gateway.nginxPolicies.enabled=true`:

- `ClientSettingsPolicy` (`body.maxSize`) keeps the OTLP ingest body limit in
  lock step with `supervisor.config.otel.ingestMaxBodySize`;
- `ProxySettingsPolicy` (`timeout.read`/`send`) keeps long-lived `/v1/engine`
  WebSockets, SSE streams, and cold Zot pulls open;
- `SnippetsPolicy` (`context: stream.server`, `proxy_timeout <sessionTimeout>;`)
  raises NGINX's 10m stream default for the data plane — the Gateway API
  equivalent of the `TransportServer` `sessionParameters.timeout`.

Disabling the block keeps the routes portable to non-NGF controllers.

### D5 — cert-manager contract

cert-manager's Gateway API support must be enabled cluster-wide (CRDs installed
before cert-manager; `config.enableGatewayAPI: true` on v1.15–1.20,
`config.gatewayAPI.enabled: true` on v1.21+). Setting
`gateway.annotations: {cert-manager.io/cluster-issuer: <issuer>}` lets the
cert-manager gateway-shim create the `Certificate`s for the HTTPS listeners;
`dataCert.enabled: true` keeps issuing the data-host `Certificate` from the
existing template. The ClusterIssuer's HTTP-01 solver points at the Gateway
(`http01.gatewayHTTPRoute.parentRefs`, `sectionName: http`).

## Alternatives considered

- **`ingress2gateway`-only migration** — rejected: it produces static routes
  with no data-plane passthrough equivalent and no cert-manager shim wiring,
  so it cannot express the mTLS data plane or the ACME challenge listener.
- **Envoy Gateway** — viable, but NGF is the natural successor to the existing
  NGINX-based exposition and its `BackendTLSPolicy` / policy attachments map
  cleanly onto the current timeouts and body-size tuning.
- **`ListenerSet` for the data plane** — deferred: `ListenerSet` is newer and
  not uniformly supported; a single Gateway with a `TLS` `Passthrough` listener
  covers the same need today.
- **Rendering a `Certificate` per listener** — rejected: the cert-manager
  gateway-shim already derives them from `gateway.annotations`, so no extra
  templates or `ReferenceGrant` are needed (single-namespace references).

## Consequences

- Chart: a new `gateway.*` section, five new templates
  (`gateway.yaml`, `gateway-httproute.yaml`, `gateway-tlsroute.yaml`,
  `gateway-extra-routes.yaml`, `gateway-nginx-policies.yaml`), new helpers
  (`gatewayName`, `gatewayNamespace`, `dexServiceName`,
  `registryMirrorExternalHost`, `dataTLSSecretName`), and precedence-aware
  `publicUrl` / `dataHostname` / `dataplaneTLS*` helpers. Default rendering is
  unchanged.
- Operator prerequisites (documented, not automated): install NGF ≥ 2.9 (with
  `nginxGateway.snippets.enable=true` for the stream timeout), enable
  cert-manager Gateway API support, and point the issuer solver at the Gateway.
- The mirror and Dex routes are in scope; Grafana/MinIO consoles and other
  subcharts are out of scope (reach them via `kubectl port-forward`).
- `gateway.create=false` requires `gateway.parentRef.name` (fail-closed).

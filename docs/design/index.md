# Architecture Decision Records — dagger-kubernetes

This directory contains Architecture Decision Records (ADRs) for the
dagger-kubernetes supervisor control plane. Each ADR documents a significant
architectural choice, the alternatives considered, and the rationale for
the decision.

| #    | Title                                                            |
|------|------------------------------------------------------------------|
| 001  | [Mandatory library stack](ADR-001-mandatory-stack.md)            |
| 002  | [net/http to Hertz migration](ADR-002-net-http-to-hertz-migration.md) |
| 003  | [SSE via Hertz native](ADR-003-sse-via-hertz-native.md)          |
| 004  | [Per-version StatefulSet autoscaler](ADR-004-per-version-statefulset-autoscaler.md) |
| 005  | [Embedded minting CA](ADR-005-embedded-minting-ca.md)            |
| 006  | [OCI registry cache backend](ADR-006-oci-registry-cache-backend.md) |
| 007  | [Outbound HTTP clients](ADR-007-outbound-http-clients.md)        |
| 008  | [Prometheus metrics DI](ADR-008-prometheus-metrics-di.md)        |
| 009  | [Clean architecture layering](ADR-009-clean-architecture-layering.md) |
| 010  | [SQLite-backed multi-user RBAC](ADR-010-sqlite-multiuser-rbac.md) |
| 011  | [Engine proxy, CA, and engine.toml config injection](ADR-011-engine-env-ca-config-injection.md) |
| 012  | [MagicCache dashboard — cache stats, services status, GC, and purge](ADR-012-magiccache-dashboard.md) |
| 013  | [Connect-env UI menu](ADR-013-connect-env-menu.md) |
| 014  | [Registry proxy — token control + multi-registry load balancing](ADR-014-registry-proxy-token-loadbalancing.md) |
| 015  | [Raft replaces SQLite](ADR-015-raft-replaces-sqlite.md) |
| 016  | [Raft multi-node TLS + StatefulSet](ADR-016-raft-multinode-tls.md) |
| 017  | [Auth always enforced + multi-provider OAuth](ADR-017-auth-always-enforced-and-multi-provider-oauth.md) |
| 018  | [Pipeline history auto-purge + manual purge](ADR-018-history-purge.md) |
| 019  | [Client disconnect detection](ADR-019-client-disconnect-detection.md) |
| 020  | [Log auto-follow UX](ADR-020-log-autofollow-ux.md) |
| 021  | [Self-hosted pipeline view URL](ADR-021-pipeline-view-url.md) |
| 022  | [OAuth group allowlists + regex group mapping](ADR-022-oauth-group-allowlists-and-regex-mapping.md) |
| 023  | [On-the-fly Dagger CLI provisioning addon](ADR-023-cli-provisioning.md) |
| 024  | [CI integration — nested Dagger step view](ADR-024-ci-nested-steps.md) |
| 025  | [Robust config decoding — extended durations and dot-safe map keys](ADR-025-config-decode-durations-dotted-keys.md) |
| 026  | [Replicated session leases + leader-routed Services](ADR-026-replicated-session-leases.md) |
| 027  | [OAuth group-membership revalidation & token invalidation](ADR-027-oauth-membership-revalidation.md) |
| 028  | [Single global BuildKit cache (tag `cache`)](ADR-028-global-cache.md) |
| 029  | [Raft FQDN-only discovery + NodeLocal DNSCache bypass](ADR-029-raft-fqdn-only-discovery.md) |
| 030  | [Upstream OAuth group display + mapping diagnostics](ADR-030-upstream-oauth-group-display-and-diagnostics.md) |
| 031  | [OAuth `admin_groups` → `RoleAdmin` promotion](ADR-031-oauth-admin-groups-role-promotion.md) |
| 032  | [Engine local-cache purge (per version, live dagql prune)](ADR-032-engine-cache-purge.md) |
| 033  | [Local image cache (Zot on-demand mirror)](ADR-033-local-image-mirror.md) |
| 034  | [Admin image-cache management (list / prune / prune-all)](ADR-034-admin-image-cache-management.md) |
| 035  | [OAuth group-mapping auto-creates missing groups with a default engine limit](ADR-035-oauth-group-mapping-auto-create.md) |
| 036  | [Config-driven project → group mapping](ADR-036-config-project-group-mapping.md) |
| 037  | [Pipeline-view observability — internal-span filtering, service detection, exec logs, trace-scoped engine metrics](ADR-037-pipeline-view-observability.md) |
| 038  | [Pipeline tree drill-down, zoom, and subtree-scoped log search](ADR-038-pipeline-tree-zoom-search.md) |
| 039  | [CI timeout is opt-in, live Dagger output, and live pipeline-view URL](ADR-039-ci-timeout-live-streaming.md) |
| 040  | [Frontend is Nuxt 4 + Nuxt UI v4 (static SPA)](ADR-040-frontend-nuxt4-nuxt-ui.md) |
| 041  | [Per-pod leader-forward proxy (drop label-based leader routing)](ADR-041-per-pod-leader-forward-proxy.md) |
| 042  | [Runner-fleet metrics + storage usage (cAdvisor, 15m rolling window)](ADR-042-runner-fleet-metrics-storage.md) |
| 043  | [Engine (fleet runner) image pulled through the cache mirror](ADR-043-engine-image-via-cache.md) |

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

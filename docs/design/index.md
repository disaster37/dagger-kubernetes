# Architecture Decision Records — dagger-cache

This directory contains Architecture Decision Records (ADRs) for the
dagger-cache supervisor control plane. Each ADR documents a significant
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

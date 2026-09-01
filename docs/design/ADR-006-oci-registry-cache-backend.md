# ADR-006: OCI Registry Cache Backend

**Status:** Accepted · **Date:** 2025-06-01 · **Author:** dagger-kubernetes team

## Context

The Dagger CLI uses BuildKit's remote cache feature to share layer blobs
across CI runs. The cache must be fast, reliable, and compatible with the
standard BuildKit cache configuration format.

## Decision

Support two cache backends:

1. **Registry (OCI)** — a standard OCI Distribution-compatible registry
   (`registry:2` or any OCI-compliant registry). BuildKit cache blobs are
   stored as OCI manifest layers under a single global tag `cache`.
   Default: `cache.reg/dagger-cache:cache`.

2. **S3** — an S3 bucket. BuildKit's `type=s3` cache config is generated
   from `cache.s3.bucket` and `cache.s3.region`.

The cache ref uses the fixed global tag `cache`, shared across all engine
versions. BuildKit cache is content-addressed, so cross-version sharing is
safe.

## Rationale

- OCI registries are well-understood, easy to deploy (single Docker
  container), and supported by every major CI platform.
- S3 is a natural choice for cloud-native deployments.
- Version-tagged refs are no longer needed: BuildKit cache is
  content-addressed, so a single global tag is safe across versions.

## Consequences

- The `cache.Backend` struct generates the `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`
  value that the client uses.
- When `cache.public_host` is set, the registry host in the cache ref is
  replaced with the public hostname (for external access through the
  Supervisor).

> **Superseded in part by ADR-014:** the cache ref now targets the
> Supervisor proxy vhost by default; `cache.public_host` is the dedicated
> cache vhost, and the Supervisor controls registry credentials and
> load-balances across backend registries.
>
> **Superseded in part by ADR-028:** the version-tagged refs and
> `cache.ref_per_version` flag are removed; the cache uses a single global
> `:cache` tag shared across all engine versions.

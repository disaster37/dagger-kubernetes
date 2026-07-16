# ADR-006: OCI Registry Cache Backend

**Status:** Accepted · **Date:** 2025-06-01 · **Author:** dagger-cache team

## Context

The Dagger CLI uses BuildKit's remote cache feature to share layer blobs
across CI runs. The cache must be fast, reliable, and compatible with the
standard BuildKit cache configuration format.

## Decision

Support two cache backends:

1. **Registry (OCI)** — a standard OCI Distribution-compatible registry
   (`registry:2` or any OCI-compliant registry). BuildKit cache blobs are
   stored as OCI manifest layers under a version-tagged repository ref.
   Default: `cache.reg/dagger-cache:V<slug>`.

2. **S3** — an S3 bucket. BuildKit's `type=s3` cache config is generated
   from `cache.s3.bucket` and `cache.s3.region`.

The cache ref is derived from the engine version and the configured
registry. The `cache.ref_per_version` flag (default `true`) appends a
`:V<maj>-<min>-<patch>` tag to prevent cross-version cache pollution.

## Rationale

- OCI registries are well-understood, easy to deploy (single Docker
  container), and supported by every major CI platform.
- S3 is a natural choice for cloud-native deployments.
- Version-tagged refs prevent accidental cache poisoning between Dagger
  engine versions.

## Consequences

- The `cache.Backend` struct generates the `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`
  value that the client uses.
- When `cache.public_host` is set, the registry host in the cache ref is
  replaced with the public hostname (for external access through the
  Supervisor).

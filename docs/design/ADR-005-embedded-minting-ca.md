# ADR-005: Embedded Minting CA

**Status:** Accepted · **Date:** 2025-06-01 · **Author:** dagger-cache team

## Context

The data plane uses mutual TLS (mTLS) to authenticate clients. Each client
lease requires a short-lived client certificate signed by a trusted CA.
The CA must be available at startup and must persist across restarts.

## Decision

Use an **embedded minting CA** that generates its own key pair at first
startup and persists it:

- On first start: generate an ECDSA P-256 CA key and self-signed
  certificate, write them to `ca_path` as `ca.crt`/`ca.key`.
- On subsequent starts: load the existing `ca.crt`/`ca.key` from disk.
- In a multi-node Kubernetes deployment (ADR-016), the embedded provider
  shares the CA across pods via the `ca.minting_ca_secret` Secret (bootstrap
  on ordinal 0, poll otherwise) so every pod mints/trusts the same CA; the
  `ca_path` files are kept as a local cache.
- `MintClientCert`: sign a client certificate with the CA key for the
  requested `commonName`, with a configurable TTL (`client_cert_ttl`).
- Three TLS provider implementations: `embedded` (goca-based),
  `cert-manager` (external), `external` (user-managed files).

## Rationale

- No external dependency on cert-manager for basic deployments.
- Short-lived client certs (default 2h) limit the blast radius of a
  compromised cert.
- The CA can be rotated by deleting the persisted files and restarting.

## Consequences

- CA persistence path must be a writable volume (bind-mount or PVC).
- The CA certificate is distributed to clients as part of the lease
  response (`SerializableCertificate`).
- The `ca_path` defaults to `/var/lib/dagger-cache/ca`.
- In multi-node, the `ca.minting_ca_secret` Secret holds the CA **private
  key** (any pod may mint client certs) and must be RBAC-restricted to the
  supervisor ServiceAccount.

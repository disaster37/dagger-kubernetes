# ADR-050: Adopt shared `go-raft`, `go-auth`, and `go-secguard` libraries

- **Status:** accepted
- **Date:** 2026-11-15
- **Deciders:** dagger-kubernetes maintainers
- **Supersedes:** ADR-016 R5 ("CA rotation out of scope")
- **Related:** ADR-005, ADR-009, ADR-015, ADR-016, ADR-029, ADR-041

## Context

`dagger-kubernetes` grew three large, security-sensitive subsystems that
duplicate functionality now published as reusable libraries:

1. **Raft lifecycle** — node bootstrap, BoltDB + snapshot wiring, StatefulSet
   FQDN / static / single-node peer discovery, dynamic mTLS + retrying stream
   layer, Kubernetes Secret / file / local CA bootstrap, stale-PVC generation
   fencing, autopilot dead-server tracking, and Raft Prometheus metrics.
2. **Auth & cryptography** — HS256 JWT signing/parsing, `dct_` API token
   lifecycle, AES-256-GCM at-rest sealing, OAuth credential sealing, regex
   group mapping, bcrypt hashing/timing-equalization, cookie specification
   builders, and CSPRNG OAuth state nonces.
3. **Security guardrails** — per-account exponential lockout (CWE-307),
   credential-stuffing IP bans, per-IP auth DoS rate limiting, spoof-proof
   client IP extraction (CWE-290/CWE-348), and CSRF mutation-origin checks
   (CWE-352).

Maintaining three parallel implementations of the same primitives multiplies
the audit surface and lets fixes drift between projects.

## Decision

Replace the bespoke implementations with the shared libraries:

- **`github.com/disaster37/go-raft v1.0.0`** — Raft node lifecycle, discovery,
  transport, PKI/keyring, storage, autopilot, observability, and
  `httpext.Forwarder` for follower→leader proxying.
- **`github.com/disaster37/go-auth v1.0.0`** — JWT, API tokens, at-rest
  crypto, OAuth credentials/group mapping/HTTP client, password hashing, and
  HTTP cookie specs.
- **`github.com/disaster37/go-secguard v1.0.0`** — the unified `*secguard.Guard`
  facade (lockout, ban tracker, rate limiters, real-IP extraction, security
  headers, mutation-origin checks).

The Go toolchain is bumped to **1.27.0** (all three libraries require it) and
`golangci-lint` to **v2.14.0**.

### Preserved contracts

- **JWT wire format:** `jwt.Config{UseHKDF: false}` keeps raw-secret HS256
  signing so active cluster sessions survive the upgrade.
- **Ciphertexts:** `crypto.AESGCMSealer.OpenString` opens both the new JSON
  envelope and the legacy `base64(nonce||ciphertext||tag)` format.
- **HTTP routes/payloads, SSE & OTLP streaming, cookie names/flags, Prometheus
  metric names, and Raft snapshot compatibility** are unchanged.
- **Clean Architecture (ADR-009):** `internal/domain/` stays stdlib-only; the
  shared libraries are imported only by `service`, `repository`, `handler`,
  `config`, and `cmd`.
- **Hertz-only HTTP (ADR-002):** `internal/handler/` never imports `net/http`;
  the `httpext.Forwarder` adapter builds `map[string][]string` headers with
  `net/textproto.CanonicalMIMEHeaderKey`.

### Deliberately out of scope

- The relational `FSM` (`fsm.go`) with secondary indices, FK cascades, ADR-049
  CAS `ExpectedRevision`, and `ErrLastAdmin` guards is retained; the library's
  KV FSMs are not used.
- The unified `config.app.yaml` loader (flat `raft.replicas`/`raft.peers`
  keys) is retained; only the weak-typing decode hooks are delegated to
  `go-raft/config`.
- `go-auth`'s root `Service`, `password.Service`, and OAuth provider services
  predate ADR-049 and lack atomic `CommitOAuth` CAS revisions, so the
  supervisor's own orchestration is retained.
- `net/http` server adapters from `go-auth`/`go-secguard` are forbidden in
  `internal/handler/`.

## Consequences

- **Positive:** one audited implementation of Raft, auth crypto, and
  guardrails; automatic security fixes flow in via dependency bumps; the
  supervisor's own code shrinks substantially.
- **Negative:** a new external dependency surface; the toolchain floor rises to
  Go 1.27; behavioral parity must be proven by the existing test suite plus new
  guardrail tests.
- **Migration:** the config, security-guardrail, auth, Raft store, Minting CA
  rotation, and dynamic per-handshake TLS layers are all migrated in this
  changeset. `internal/repository/raft_store.go` now wraps `go-raft`'s `Node`
  (storage, transport, discovery, autopilot, keyring rotation, join loop);
  `minting_rotator.go` provides the phased dual-trust Minting CA rotation and
  the `cert-manager`/`external` `FileWatcher` hot-reload; the local
  `raft_discovery.go`, `raft_streamlayer.go`, `raft_tls.go`,
  `raft_autopilot.go`, and `observ/raft_metrics.go` are deleted.

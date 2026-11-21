# ADR-052: SemVer pre-releases, Org tenancy, and 1.0-beta engine provisioning

## Status
Accepted

## Context
Dagger Cloud 1.0-beta uses SemVer pre-releases (`v1.0.0-beta.27`) and an
`Org → Group → Project` hierarchy. Existing `0.21.x` CI pipelines must keep
working during the upgrade.

## Decision
- `domain.Version` implements SemVer 2.0 precedence including pre-release and
  build metadata; slugs replace dots/build separators with hyphens.
- `cli.include_prereleases` (default false) controls `latest`; explicit
  pre-release downloads and `POST /v1/engines` always accept them.
- Add `Org` and `OrgMembership` (`org_admin`, `member`) to the Raft FSM with
  new command kinds appended after existing slots. Legacy rows with an empty
  `OrgID` migrate deterministically into the bootstrap `default` org.
- `fleet.isolation` (`per_org` default, `shared`) scopes StatefulSets,
  Services, PVCs, and remote-cache namespaces.
- Engine pods receive the union Minting CA bundle via a projected Secret and
  `SSL_CERT_FILE`, plus in-cluster control/data URLs to avoid hairpin NAT.

## Consequences
Pre-release engines can be provisioned without breaking stable pipelines, and
multi-tenant deployments get isolated fleets and caches.

## Follow-up (2026-11-18)
- **Pre-release allowlist forms.** Each `version.allowlist` entry is parsed into
  one of three rules: a minor series (`"1.0"` matches any `v1.0.x`, including
  `v1.0.0-beta.*`), a pre-release track (`"1.0.0-beta"` matches
  `v1.0.0-beta.*`), or an exact version (`"v1.0.0-beta.28"`). All remain
  subject to `version.floor` (so `floor: "v1.0.0"` still rejects
  `v1.0.0-beta.28`). `cli.include_prereleases` continues to govern only the
  `latest` resolution.
- **Host-based internal data host + SNI routing.** The internal vs public data
  host is now selected from the request `Host` (matching
  `server.internal_control_url`'s hostname) rather than the peer IP, and the
  follower→leader forward hop preserves the original `Host` via an
  internal-listener-only header. `:8443`/`:8080` serve the minting-CA leaf
  (with SANs derived from the internal control/data hosts) for SNIs matching
  the exact configured internal control/data host forms (short `<svc>`,
  `<svc>.<ns>.svc` and `<svc>.<ns>.svc.<clusterDomain>`); every other SNI,
  including arbitrary `.svc` names and the public host, keeps the public
  certificate.
- **Per-org fleet naming.** Scope-aware engine naming
  (`dagger-engine-<org>-<version>` in `per_org`, `dagger-engine-<version>` in
  `shared`) is applied by the sweeper, cache purge, and metrics, so idle
  engines are reclaimed by their real StatefulSet name.


## Extended by ADR-058

- Group and project names are unique per `(OrgID, lower(Name))`, not globally.
- Every organization (including the bootstrap `"default"` slug) uses a generated
  `org-<16hex>` storage UID recorded at `meta["bootstrap_org_id"]`; `Org.Name`
  is immutable.
- Engine tokens are per organization (`Secret/dagger-engine-token-<orgID>`)
  and injected via `SecretKeyRef`; no shared plaintext token is injected.

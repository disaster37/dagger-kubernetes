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

# ADR-058 — Organization as root namespace: tenancy, quotas, and storage

Status: Accepted (extends ADR-052 and ADR-055)

## Context

The initial tenancy cut (ADR-052) left `Organization` as a sidecar beside the
legacy global tables. Groups and projects were globally name-unique, services
could write rows with an empty `OrgID`, one supervisor-minted engine token was
injected into every organization's engine pods, and several handlers fell back
silently to a hardcoded `"default"` tenant. Remote-cache, artifact, and OTLP
archive object keys were not consistently tenant-prefixed.

## Decision

1. **Unified root hierarchy.** `Org → Group → Project` is mandatory. Every
   group and project write carries a non-empty `OrgID`; names are unique per
   `(OrgID, lower(Name))`; assigning a project to a group from another
   organization is rejected in both the service and the FSM. Removing an org
   member cascades only that organization's group memberships and live leases.
2. **Organization creation is administrator-only.** `POST /api/v1/orgs` and
   GraphQL `createQuickstartOrg` require the global `RoleAdmin`. A new user with
   zero memberships lands in the UI "No organization access" state until an
   administrator or an OAuth group mapping adds them.
3. **Uniform storage UIDs and immutable slugs.** Every organization — the
   bootstrap one included — has a generated `ID = "org-<16hex>"`
   (`^org-[0-9a-f]{16}$`) and a separate immutable `Name` slug. The bootstrap
   organization's slug is `"default"` and its UID is recorded once in
   `meta["bootstrap_org_id"]`; it can never be deleted or renamed. `Org.ID`
   keys FSM rows, Kubernetes resource names, S3 prefixes, bbolt buckets,
   `org:<id>` token subjects, and Prometheus labels. Because the branch that
   introduced organizations was never merged, and `main` had no organizations
   or tenant S3 prefixes, there is no migration cost.
4. **Deterministic Raft bootstrap.** `newState()` starts with zero organizations
   and calls neither `crypto/rand` nor a placeholder-org builder. The leader
   appends `kindBootstrapOrg = 45`, which atomically sets
   `meta["bootstrap_org_id"]`, stores the bootstrap org, ensures its `"default"`
   group, and normalizes all pre-upgrade `main` rows (`OrgID == ""`) into it.
   A second proposal across a term change is a deterministic no-op. A follower
   that joins via `InstallSnapshot` restores `Orgs` and `Meta` from the
   snapshot, so all peers converge on one UID.
5. **Display-name precedence.** Only `org.default.display_name` is
   configurable (Viper/env/Helm). The value last applied from configuration is
   stored at `meta["default_org_display_name"]`: an unchanged Helm value leaves
   a UI edit intact across restarts; a changed Helm value wins. `Org.Name`
   never changes.
6. **Fail-closed resolution.** `domain.DefaultOrgID` and every
   `if orgID == "" { orgID = … }` fallback are deleted. Requests resolve an org
   from the `dag_<slug>_` token, `:org`, `X-Dagger-Org`, or `?org=`, verified
   against live membership. Without a selector, resolution succeeds only when
   unambiguous (one accessible org for a user, one cluster org for an admin);
   two or more returns `400 ambiguous organization`, zero returns `403`.
7. **Universal org quotas binding every role.** `OrgQuotas` carries soft/hard
   ceilings for groups, projects, members, concurrent sessions, engines, and
   cache bytes plus per-org pipeline retention. Zero means unlimited/inherit.
   Soft limits admit with `X-Dagger-Quota-Warning`, a counter, and a warning
   log; hard limits reject with `429` (and a WebSocket error envelope) for
   `RoleAdmin` as well as ordinary users. `RoleAdmin` retains only the legacy
   per-group `max_runner_sessions` bypass. Entity caps are also enforced
   deterministically inside the serialized Raft apply; runtime caps are
   serialized per organization by a striped admission mutex. The supported
   escape hatch is `PUT /api/v1/orgs/:org/quotas`, audited at info level.
8. **Per-organization runner fleets.** Engine StatefulSets, Services, and PVCs
   are named from `org.ID` (`dagger-engine-org-<16hex>-<vslug>`, capped at 52
   characters for the pod revision-hash label) and labelled with the org ID and
   slug. Each org's `dag_<slug>_` token lives in
   `Secret/dagger-engine-token-<orgID>` and is injected via `SecretKeyRef`;
   plaintext tokens never appear in pod specs. `fleet.isolation: "shared"` is
   deprecated: it logs a warning, forbids a second organization, and collapses
   to `_shared` namespaces.
9. **Tenant object-storage layout.** All tenant objects live under
   `orgs/<org_id>/{<remote.prefix>,artifacts,otlp-archive}/…` with a per-org
   remote-cache index checkpoint (`orgs/<org_id>/<remote.prefix>/index/checkpoint.json`).
   Engines receive only short-lived single-key presigned URLs. `OrgObjectStoreResolver`
   is the extension point for bucket/credential-per-org deployments;
   `SharedBucketResolver` is the default. `cli-cache` and the Zot
   `image-cache` stay cluster-global. No `orgs/default/...` compatibility
   paths exist.
10. **Org-scoped API and UI.** Unscoped `/api/v1/groups` and `/api/v1/projects`
    are removed permanently (no aliases). Group, project, member, quota,
    secret, fleet, and remote-cache operations live under `/api/v1/orgs/:org/…`.
    The UI uses a `useOrgStore`, a header `OrgSwitcher` with a deterministic
    alphabetical fallback, grouped navigation, and `/orgs/[org]/*` pages.

## Consequences

- Single-tenant deployments remain zero-config: one auto-bootstrapped org, all
  quotas unlimited, and unambiguous auto-selection.
- Multi-tenant deployments get strict isolation (cache, artifacts, traces,
  fleets, tokens, quotas) with administrator-controlled tenant creation.
- Rolling upgrades from `main` need no data migration; a follow-only pod reports
  `503 organization bootstrap in progress` until the new leader commits
  `kindBootstrapOrg`.
- Operators that need more capacity raise the org quota explicitly; there is no
  role-based exemption.

## Follow-ups

- **Membership creation times on pre-upgrade state.** The replicated state keeps
  the direct-membership role map (`org_members` snapshot key) unchanged and
  stores timestamps in a parallel `orgMemberSince` map, persisted as the
  optional snapshot field `org_member_since`. Snapshots written before that
  field existed (through Helm revision 128) decode with an empty map and are
  backfilled on restore — every membership in `orgMembers` without a timestamp
  gets the bootstrap organization's `CreatedAt`, the best-known lower bound.
  A role change never rewrites the original timestamp.

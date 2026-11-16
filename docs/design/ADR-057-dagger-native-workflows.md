# ADR-057: Dagger-native workflows, artifacts, and approvals

## Status
Accepted

## Context
Teams need a Dagger-native CD replacement for Jenkins/GitHub Actions/GitLab CI
without arbitrary host-shell execution.

## Decision
- Parse `.dagger/workflows/*.y{a,}ml` hot during each commit's load phase.
- Support push/PR/tag/release/schedule/workflow_dispatch/workflow_call/artifact
  triggers and `after_checks` (default true).
- Evaluate `${{ ... }}` with `expr-lang/expr` in a sandbox; provide `file()`
  (jailed via `os.Root`, 1 MiB cap), SemVer helpers, `slug`, `coalesce`, and
  `next_semver`.
- Execute `call:` jobs as ephemeral K8s Jobs; execute cross-repo `trigger:`
  jobs natively in the leader state machine.
- Store declared artifacts in S3 with SHA-256 verification and fire matching
  `on: artifact` workflows.
- Protected environments pause in `waiting_approval` with zero engine pods,
  enforce approver groups/`org_admin`, and inject environment secrets only
  after approval.
- Arbitrary host-shell `run:` steps are explicitly out of scope.

## Consequences
Workflows are portable, secret-safe, and auditable, with no host-shell escape
hatch.

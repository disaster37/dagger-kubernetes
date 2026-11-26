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

## Update — residual-risk remediation (PR #70 review)
- `file()` requires an explicit repository root (never supervisor `"."`),
  supports `.json` and 1-arg plain text, and is jailed with `os.OpenRoot`.
- The expression context includes full `git.*`, `run.*`, `inputs.*`,
  `needs.<job>.outputs.*`, and `artifact.*`; `next_semver` handles tag,
  release, PR, default-branch (`rc` vs release), and feature branches.
- Runs are replicated in Raft with atomic monotonic numbers; `spec` is frozen
  so `Approve` survives leader failover and repo edits.
- `Approve`/`Reject` enforce `org_admin`/approver-group RBAC,
  `prevent_self_approval`, and `approval_timeout`, then resume the DAG.
- Cross-repo `trigger:` jobs run natively on the leader with `wait`/`timeout`
  and zero K8s pods held while waiting.
- `robfig/cron/v3` schedules and `on: artifact` listeners are evaluated on the
  leader; `after_checks` workflows start only after checks succeed.
- Workflow Job completion callbacks require a per-run `X-Dagger-Run-Token`
  header in addition to the org bearer token. Only the token's SHA-256 hash is
  replicated on `WorkflowRun`; the plaintext is injected as
  `DAGGER_RUN_TOKEN` into the Job's ephemeral Secret.
- The plaintext token lives only in leader memory. After a leader restart the
  plaintext is lost, so the next Job dispatch re-mints a token and persists its
  hash; the previous hash is retained in `WorkflowRun.PreviousRunTokenHash` so
  in-flight Jobs dispatched before the restart can still report completion.
  Minting is serialized to avoid two concurrent dispatches minting different
  tokens for the same run.

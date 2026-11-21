# ADR-056: Cloud Checks and multi-forge integration

## Status
Accepted

## Context
Cloud Checks replaces external CI for repository checks across GitHub, GitLab,
and Gitea/Forgejo.

## Decision
- Implement `domain.ForgeProvider` adapters in `repository` (outbound
  `net/http` per ADR-007) for GitHub (token + App/Check Runs), GitLab, and
  Gitea/Forgejo.
- Register webhooks for push/tag/PR/release and verify per-source HMAC/token
  secrets. Invalid signatures increment `webhook_signature_invalid` and never
  call `recordTokenFailure`.
- Store write-only AES-256-GCM secrets at org/project/environment scope with
  scope-bound AAD.
- Dispatch check runs as `batch/v1` Jobs labelled
  `dagger-kubernetes.io/check-run-id`; the `dagger-kubernetes-ci check-runner`
  subcommand clones at the target SHA, runs `dagger check -l`, and executes
  each check. Zero checks is a valid GitOps-only result.
- Fork PRs hold in `waiting_approval` and receive no org/project secrets.

## Consequences
One supervisor can drive checks for multiple forges without leaking secrets or
auto-banning shared NAT egress IPs.

## Update — residual-risk remediation (PR #70 review)
- Check Jobs receive `DAGGER_CLOUD_TOKEN`/`DAGGER_KUBERNETES_TOKEN`,
  `DAGGER_RUN_TOKEN`, and `GIT_CLONE_TOKEN` from an ephemeral `check-env-*`
  Secret referenced via `EnvFrom` (never argv). The Secret is owned by the Job
  and garbage-collected with it; Jobs set `TTLSecondsAfterFinished`.
- `check-runner` reports `/load` (checks + workflow files + jailed context
  files) and `/complete` (per-check state/trace IDs) back to the supervisor.
  Both callbacks require the per-run `X-Dagger-Run-Token` header in addition to
  the org bearer token. The supervisor stores only the SHA-256 hash on the
  replicated `CheckRunRecord` (never in API JSON) and mints a fresh token on
  every dispatch, fork-PR approval, and rerun. Workflow job completion uses the
  same per-run token pattern (`WorkflowRun.RunTokenHash`).
- Live and terminal states are reported to the forge (`SetCommitStatus` or
  `CreateCheckRun`), with `/pipelines/<traceID>` links.
- Fork PRs hold in `waiting_approval`; members may approve secretless runs,
  only `org_admin`/global `admin` may set `include_secrets`.
- A leader reconciler marks crashed/timed-out Jobs `failed` and reports
  `CheckFailure`.
- Org-scoped `dag_<org>_` tokens are admitted by `QuotaService` against the
  org's groups (or fleet capacity for a fresh org).

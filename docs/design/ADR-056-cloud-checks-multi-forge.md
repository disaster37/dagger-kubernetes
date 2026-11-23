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

## Update — enable-checks end-to-end (event policy, gating, runner hardening)
Live validation surfaced several gaps between the design and a working
push → check-run → `dagger call` flow. This update closes them.

- **Webhook event policy.** Deliveries are classified before any write.
  `domain.ClassifyAndParseForgeWebhook` returns a `WebhookDisposition`:
  actionable (push/tag/PR/release with a supported action) or ignored with a
  reason (`ping`, `unsupported_event`, `unsupported_action`, `ref_deleted`).
  Ignored deliveries return `200 {"status":"ignored","reason":...,"event":...}`
  and never touch projects, records, dispatch, or the forge. GitHub hooks are
  registered for `["push","pull_request","release"]` (push covers branch+tag),
  matching Gitea/GitLab; the handler-side classifier stays authoritative for
  pre-existing `["*"]` hooks.
- **`checks.enabled` / `workflows.enabled` gating.** `ChecksService` and
  `WorkflowService` default to enabled and expose `WithEnabled`. When checks
  are disabled the webhook verifies the signature then returns
  `ignored{checks_disabled}` with zero writes, and REST rerun/approve return
  `400`; reads stay `200`. When workflows are disabled run creation is gated
  in `Start`/`Dispatch`/`OnChecksSucceeded`; reads stay. The reconciler and
  scheduler only start when their subsystem is enabled.
- **Namespace fallback chain.** `resolvePodNamespace` resolves the runner
  namespace as configured → `POD_NAMESPACE` → the projected ServiceAccount
  namespace file → `dagger-kubernetes`. `cmd/api` passes
  `effectiveNamespace(checks|workflows.namespace, fleet.namespace)` so the
  runner lands in the release namespace (where its RBAC lives) by default.
- **Runner image + fixes.** A dedicated `runner` Dockerfile stage ships
  `git`, `ca-certificates`, and `dagger-kubernetes-ci` (the Dagger CLI is
  provisioned on demand). The runner image defaults to the supervisor image.
  Four correctness fixes: (A) `domain.ForgeCloneURL` derives the HTTPS clone
  URL from the provider/base URL and is passed as `--clone-url` (a bare
  `owner/repo` never becomes `https://owner/repo`); (B) the runner resolves the
  absolute `dagger` path (exec.LookPath ignores `cmd.Env`) and builds the CLI
  download URL from the supervisor URL to avoid hairpin NAT; (C) both
  `DAGGER_ENGINE=cloud` and `_EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self`
  are set (0.21.x ignores the former), plus live-trace and `DAGGER_ORG` vars;
  (D) the minting-CA volume projects only `ca.crt` (never `ca.key`) and Job
  pods set `AutomountServiceAccountToken=false`.
- **Failed-dispatch transition.** A dispatch error transitions the record to
  `failed` with the error and reports `CheckFailure` to the forge, so a run is
  never stranded in `running`. A duplicate delivery for an already-active run
  is an idempotent no-op (no token rotation, no re-dispatch). A rerun replaces
  the terminal Job and its env Secret so the fresh run token takes effect.
- **DELETE endpoint.** `DELETE /api/v1/checks/runs/:id` (org admin or global
  admin) removes a run and is idempotent (`204`).

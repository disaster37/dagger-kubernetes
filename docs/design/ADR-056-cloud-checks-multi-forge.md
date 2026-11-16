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

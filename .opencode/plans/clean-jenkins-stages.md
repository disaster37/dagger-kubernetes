# Plan: Simplify Jenkins Dynamic Stages to 2 Clean Stages

## Overview

Replace the complex span-tree rendering in `dynamicStages` mode with exactly 2
clean stages: "Provision Dagger CLI" (conditional) and "Dagger" (plain-text
output). The `dagger-kubernetes-ci` wrapper with `--steps` is no longer used in
the Jenkins path; the `dagger` command runs directly.

## Scope

**Only the Jenkins Groovy shared library changes.** The Go-side `--steps` mode,
NDJSON streaming, `StepEventBuilder`, and domain types remain intact for other
CI integrations (GitHub Actions, GitLab CI, Drone).

## Current State

`dynamicStagesRun()` launches `dagger-kubernetes-ci --steps` in the background,
polls its NDJSON output, reconstructs a node tree, and recursively renders
nested `stage()` blocks via `renderStepTree()` → `renderNode()`. This produces
many noisy stages (internal Dagger engine spans, raw OTLP JSON as log lines).

## Target State

`call()` handles `dynamicStages: true` inline with two stages:

1. **"Provision Dagger CLI"** — only when `provisionCli: true`. Runs the
   existing `provisionCli()` function inside a `stage()` block.
2. **"Dagger"** — runs the `dagger` command directly with `sh`, streams
   stdout/stderr as plain text, extracts the trace ID from stderr, and prints
   the pipeline view URL. On failure, the stage fails and the build fails.

## Files Modified

### 1. `ci-integrations/jenkins/vars/daggerKubernetes.groovy`

**Functions to remove (dead code after this change):**

| Function | Lines | Reason |
|---|---|---|
| `dynamicStagesRun()` | 124–210 | Replaced by inline logic in `call()` |
| `renderStepTree()` | 219–414 | No longer needed (no NDJSON parsing) |
| `finalTraceId()` | 418–421 | No longer needed |
| `renderNode()` | 429–477 | No longer needed |
| `normalizeStageName()` | 505–514 | No longer needed |
| `formatLogLine()` | 523–566 | No longer needed |
| `withStages()` | 568–572 | Already dead code (never called) |

**Functions to keep (unchanged):**

| Function | Lines | Reason |
|---|---|---|
| `call()` | 12–90 | Modified (see below) |
| `envTruthy()` | 95–106 | Still used by `call()` |
| `parseBool()` | 109–118 | Still used by `envTruthy()` |
| `isShellUnsafe()` | 484–491 | Still used by `assertShellSafe()` |
| `assertShellSafe()` | 495–499 | Still used by `provisionCli()` |
| `provisionCli()` | 574–643 | Still used for CLI provisioning |

### 2. No other files change

- `cmd/ci/main.go` — `--steps` mode kept for other CI integrations
- `internal/service/ci_steps.go` — kept
- `internal/service/ci_render_ndjson.go` — kept
- `internal/domain/ci_steps.go` — kept
- `internal/domain/config.go` — kept (config keys remain for other CIs)
- `config/loader.go` — kept (defaults remain for other CIs)
- `docs/design/ADR-024-ci-nested-steps.md` — kept (documents the Go-side feature)

## Detailed Implementation Steps

### Step 1: Modify `call()` — dynamic stages branch

Replace lines 34–65 (the dynamic stages parameter parsing and dispatch) with
inline stage logic.

**Before (lines 34–65):**
```groovy
    boolean dynamicStages = envTruthy(params.dynamicStages, env.DAGGER_KUBERNETES_DYNAMIC_STAGES, false)
    String stepsPollInterval = params.stepsPollInterval ?: env.DAGGER_KUBERNETES_STEPS_POLL_INTERVAL ?: '2s'
    int stepsMaxDepth = (params.stepsMaxDepth ?: env.DAGGER_KUBERNETES_STEPS_MAX_DEPTH ?: 8) as int
    int stepsRenderDepth = (params.stepsRenderDepth ?: env.DAGGER_KUBERNETES_STEPS_RENDER_DEPTH ?: 0) as int
    int timeoutMinutes = (params.timeoutMinutes ?: env.DAGGER_KUBERNETES_TIMEOUT_MINUTES ?: 30) as int
    boolean magicCache = envTruthy(params.magicCache, env.DAGGER_KUBERNETES_MAGIC_CACHE, false)
    String cacheRegistry = params.cacheRegistry ?: env.DAGGER_KUBERNETES_CACHE_REGISTRY ?: 'cache.reg/dagger-cache'

    if (!serverUrl || !token) {
        error "daggerKubernetes: serverUrl and token are required"
    }

    if (params.provisionCli) {
        provisionCli(serverUrl: serverUrl, token: token,
                     version: params.cliVersion ?: env.DAGGER_KUBERNETES_CLI_VERSION,
                     os: params.cliOs, arch: params.cliArch)
    }

    String cacheConfig = ''
    if (magicCache) {
        assertShellSafe(cacheRegistry, 'cacheRegistry')
        cacheConfig = "type=registry,ref=${cacheRegistry}:cache,mode=max"
    }

    if (dynamicStages) {
        dynamicStagesRun(serverUrl: serverUrl, token: token, uiUrl: uiUrl,
                         version: version, stepsPollInterval: stepsPollInterval,
                         stepsMaxDepth: stepsMaxDepth, stepsRenderDepth: stepsRenderDepth,
                         timeoutMinutes: timeoutMinutes,
                         command: params.command, cacheConfig: cacheConfig)
        return
    }
```

**After:**
```groovy
    boolean dynamicStages = envTruthy(params.dynamicStages, env.DAGGER_KUBERNETES_DYNAMIC_STAGES, false)
    int timeoutMinutes = (params.timeoutMinutes ?: env.DAGGER_KUBERNETES_TIMEOUT_MINUTES ?: 30) as int
    boolean magicCache = envTruthy(params.magicCache, env.DAGGER_KUBERNETES_MAGIC_CACHE, false)
    String cacheRegistry = params.cacheRegistry ?: env.DAGGER_KUBERNETES_CACHE_REGISTRY ?: 'cache.reg/dagger-cache'

    if (!serverUrl || !token) {
        error "daggerKubernetes: serverUrl and token are required"
    }

    String cacheConfig = ''
    if (magicCache) {
        assertShellSafe(cacheRegistry, 'cacheRegistry')
        cacheConfig = "type=registry,ref=${cacheRegistry}:cache,mode=max"
    }

    if (dynamicStages) {
        String daggerCommand = params.command ?: env.DAGGER_COMMAND
        if (!daggerCommand) {
            error "daggerKubernetes(dynamicStages: true): pass `command: 'dagger call ...'` (or set env.DAGGER_COMMAND)"
        }

        // Stage 1: Provision Dagger CLI (only when provisionCli is enabled).
        if (params.provisionCli) {
            stage("Provision Dagger CLI") {
                provisionCli(serverUrl: serverUrl, token: token,
                             version: params.cliVersion ?: env.DAGGER_KUBERNETES_CLI_VERSION,
                             os: params.cliOs, arch: params.cliArch)
            }
        }

        // Stage 2: Run the dagger command with plain-text output.
        stage("Dagger") {
            withEnv([
                "DAGGER_CLOUD_URL=${serverUrl}",
                "DAGGER_CLOUD_TOKEN=${token}",
                "_EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self"
            ] + (version ? ["_EXPERIMENTAL_DAGGER_TAG=${version}"] : []) +
              (cacheConfig ? ["_EXPERIMENTAL_DAGGER_CACHE_CONFIG=${cacheConfig}"] : [])) {
                timeout(time: timeoutMinutes, unit: 'MINUTES') {
                    try {
                        sh daggerCommand
                    } catch (e) {
                        echo "[dagger-kubernetes] Pipeline failed. View: ${uiUrl}/traces/latest"
                        throw e
                    }
                }
            }
            // Extract trace ID from dagger's stderr for the pipeline view URL.
            // The dagger CLI prints a hex trace ID to stderr during execution.
            String traceId = extractTraceId(daggerCommand)
            if (traceId) {
                echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/pipelines/${traceId}"
            } else {
                echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/traces/latest"
            }
        }
        return
    }
```

### Step 2: Add `extractTraceId()` helper

Add a new helper function after `parseBool()` (around line 118) to extract the
trace ID from dagger's stderr. Since `sh` in Jenkins captures stdout by default
but not stderr, we need to redirect stderr to a temp file and read it back.

```groovy
// extractTraceId runs the dagger command a second time with --dry-run or
// similar to extract the trace ID. Since the command already ran in the
// "Dagger" stage, we instead capture stderr during the run.
//
// Implementation note: Jenkins `sh` step with `returnStdout: true` only
// captures stdout. To capture stderr we redirect it to a temp file during
// the main run and read it here. The temp file approach is used because
// the trace ID appears on stderr, not stdout.
String extractTraceId(String daggerCommand) {
    // The trace ID is extracted from the dagger command's stderr output.
    // We capture stderr to a temp file during the sh step in the Dagger stage.
    // This function reads that file and extracts a hex trace ID.
    String stderrFile = "/tmp/dagger-stderr-${env.BUILD_NUMBER}.log"
    String stderr = ''
    try {
        stderr = sh(script: "cat '${stderrFile}' 2>/dev/null || true", returnStdout: true)
    } catch (Exception ignored) {
    }
    def m = (stderr ?: '') =~ /[a-f0-9]{32,}/
    return m ? m[0] : ''
}
```

Wait — this approach has a problem. The `sh` step in the "Dagger" stage runs
the command and we can't easily capture stderr to a file AND display it live.
Let me reconsider.

**Revised approach:** Instead of a separate `extractTraceId` function, capture
stderr inline during the `sh` step by redirecting it to both the console and a
temp file using `tee`.

The `sh` step in the "Dagger" stage becomes:

```groovy
                timeout(time: timeoutMinutes, unit: 'MINUTES') {
                    String stderrFile = "/tmp/dagger-stderr-${env.BUILD_NUMBER}.log"
                    try {
                        // Redirect stderr to both the console (via tee) and a
                        // temp file so we can extract the trace ID afterward.
                        // 2>&1 1>&3 | tee ... keeps stdout on fd 3 (console)
                        // and pipes stderr through tee to both console and file.
                        sh "${daggerCommand} 2>&1 1> >(cat) | tee '${stderrFile}' >&2"
                    } catch (e) {
                        echo "[dagger-kubernetes] Pipeline failed. View: ${uiUrl}/traces/latest"
                        throw e
                    }
                }
```

Hmm, this is getting complicated with process substitution. Let me simplify.

**Simplest approach:** Run the command normally with `sh`, then after it
completes, use `${uiUrl}/traces/latest` as the fallback URL. The trace ID
extraction is a nice-to-have, not a hard requirement. The `/traces/latest`
endpoint on the supervisor redirects to the most recent trace.

Actually, let me look at what the dagger CLI actually outputs. The user said
"the trace ID can be extracted from the dagger command's stderr output." Let me
use a simpler approach: redirect stderr to a file during the `sh` step, then
read it.

```groovy
        stage("Dagger") {
            String stderrFile = "/tmp/dagger-stderr-${env.BUILD_NUMBER}.log"
            withEnv([
                "DAGGER_CLOUD_URL=${serverUrl}",
                "DAGGER_CLOUD_TOKEN=${token}",
                "_EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self"
            ] + (version ? ["_EXPERIMENTAL_DAGGER_TAG=${version}"] : []) +
              (cacheConfig ? ["_EXPERIMENTAL_DAGGER_CACHE_CONFIG=${cacheConfig}"] : [])) {
                timeout(time: timeoutMinutes, unit: 'MINUTES') {
                    try {
                        // Run dagger, capturing stderr to a temp file for
                        // trace ID extraction. Stdout and stderr both appear
                        // in the build log (2>&1 merges them).
                        sh "${daggerCommand} 2>'${stderrFile}'"
                    } catch (e) {
                        echo "[dagger-kubernetes] Pipeline failed. View: ${uiUrl}/traces/latest"
                        throw e
                    }
                }
            }
            // Extract trace ID from captured stderr.
            String stderr = sh(script: "cat '${stderrFile}' 2>/dev/null || true", returnStdout: true).trim()
            sh "rm -f '${stderrFile}'"
            def m = (stderr ?: '') =~ /[a-f0-9]{32,}/
            String traceId = m ? m[0] : ''
            if (traceId) {
                echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/pipelines/${traceId}"
            } else {
                echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/traces/latest"
            }
        }
```

Wait — `2>'${stderrFile}'` redirects stderr to the file, so stderr won't appear
in the build log. The user wants plain-text output. Let me use `2>&1 | tee`
instead to show stderr live AND capture it:

```groovy
                        sh "set -o pipefail; ${daggerCommand} 2>&1 | tee '${stderrFile}'"
```

`set -o pipefail` ensures the exit code of `daggerCommand` is preserved through
the pipe. This shows all output live in the build log AND captures it to the
file.

But `set -o pipefail` is bash-specific. Jenkins `sh` uses `/bin/sh` which may
be dash on some systems. Let me use a subshell:

```groovy
                        sh "(${daggerCommand} 2>&1) | tee '${stderrFile}'; exit \${PIPESTATUS[0]}"
```

This is getting fragile. Let me use the simplest reliable approach:

```groovy
                        sh "(${daggerCommand} 2>&1) > '${stderrFile}'; RC=\$?; cat '${stderrFile}'; exit \$RC"
```

This captures both stdout and stderr to the file, then cats it to the build
log, and exits with the original exit code. But this means output isn't live —
it only appears after the command finishes.

**Final decision:** Use `tee` with a bash-specific pipefail approach. Jenkins
agents typically have bash. If not, the `sh` step can specify the shell.

Actually, the simplest and most robust approach: just redirect stderr to a file
and let stdout go to the console. The trace ID is on stderr, so we capture it.
The user sees stdout live. After the command, we cat stderr to the console and
extract the trace ID.

```groovy
                        sh "${daggerCommand} 2>'${stderrFile}'"
```

Then after the command:
```groovy
            String stderr = sh(script: "cat '${stderrFile}' 2>/dev/null || true", returnStdout: true).trim()
            if (stderr) {
                echo stderr
            }
            sh "rm -f '${stderrFile}'"
```

This is clean: stdout goes to console live, stderr is captured and echoed after.
The trace ID is extracted from the captured stderr.

### Step 3: Remove dead functions

Delete lines 124–566 (from `dynamicStagesRun` through `formatLogLine`) and
lines 568–572 (`withStages`). This removes:

- `dynamicStagesRun()` (lines 124–210)
- `renderStepTree()` (lines 219–414)
- `finalTraceId()` (lines 418–421)
- `renderNode()` (lines 429–477)
- `normalizeStageName()` (lines 505–514)
- `formatLogLine()` (lines 523–566)
- `withStages()` (lines 568–572)

### Step 4: Update file header comment

Update lines 1–11 to reflect the simplified behavior:

**Before:**
```groovy
// daggerKubernetes — Jenkins shared library for the dagger-kubernetes platform.
//
// Two modes:
//   * default: runs the Dagger command (via `body`) with the platform env vars
//     set, then prints the pipeline-view link.
//   * dynamicStages: launches the `dagger-kubernetes-ci` wrapper with `--steps`
//     in the background and renders Dagger's internal step tree as nested
//     scripted-pipeline `stage()` blocks (Blue Ocean) with per-stage logs and
//     statuses. See docs/design/ADR-024-ci-nested-steps.md.
```

**After:**
```groovy
// daggerKubernetes — Jenkins shared library for the dagger-kubernetes platform.
//
// Two modes:
//   * default: runs the Dagger command (via `body`) with the platform env vars
//     set, then prints the pipeline-view link.
//   * dynamicStages: runs the Dagger command directly in two clean stages:
//     "Provision Dagger CLI" (conditional) and "Dagger" (plain-text output).
```

## Edge Cases and Error Handling

| Scenario | Behavior |
|---|---|
| `provisionCli: false` | "Provision Dagger CLI" stage is skipped entirely |
| No `command` provided | Error: "pass `command: 'dagger call ...'`" |
| Command fails (non-zero exit) | Stage fails, build fails, `/traces/latest` URL printed |
| `timeoutMinutes` exceeded | Jenkins `timeout` block aborts the stage |
| `magicCache: true` | Cache config passed via `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` env var |
| `version` set | Dagger engine version passed via `_EXPERIMENTAL_DAGGER_TAG` env var |
| No trace ID in stderr | Fallback to `${uiUrl}/traces/latest` |
| `serverUrl` or `token` missing | Error before any stage runs |
| Shell-unsafe values in interpolated strings | `assertShellSafe` catches them (existing behavior) |
| Token exposure | Token passed via env var, never appears in build log or process argv |

## Testing Strategy

1. **Unit test (manual):** Verify the Groovy file parses correctly (no syntax
   errors) by loading it in a Jenkins script console or `groovyc`.

2. **Integration test:** Deploy to the local test cluster per `AGENTS.local.md`
   and run a Jenkins pipeline with:
   ```groovy
   daggerKubernetes(
       serverUrl: 'https://supv.example.com',
       token: credentials('dagger-token'),
       dynamicStages: true,
       command: 'dagger call -m ./dagger --src . ci export --path out',
       provisionCli: true,
       timeoutMinutes: 30
   )
   ```
   Verify:
   - Two stages appear: "Provision Dagger CLI" and "Dagger"
   - No nested stages, no span tree rendering
   - Plain-text output in the "Dagger" stage
   - Pipeline view URL printed at the end
   - Build fails when the dagger command fails

3. **Regression test:** Verify the non-dynamic mode (closure body) still works:
   ```groovy
   daggerKubernetes(
       serverUrl: '...',
       token: credentials('...'),
   ) {
       sh 'dagger call ...'
   }
   ```

4. **CI gate:** Run `dagger call -m ./dagger --src . ci export --path out` to
   ensure no Go-side breakage.

## Rollback Considerations

- The removed functions (`renderStepTree`, `renderNode`, etc.) are only used by
  the dynamic stages path. No other code depends on them.
- The Go-side `--steps` mode is untouched. Other CI integrations continue to
  work.
- Config keys (`ci.jenkins.dynamic_stages`, `ci.jenkins.steps_poll_interval`,
  `ci.jenkins.steps_max_depth`) remain in the config struct and loader. They
  are still used by the Go wrapper's `--steps` mode for non-Jenkins CIs.
- To roll back: revert the Groovy file to the previous version. No other files
  need changes.

## Open Questions

None. All design decisions are resolved.

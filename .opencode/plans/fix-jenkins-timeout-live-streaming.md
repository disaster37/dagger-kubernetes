# Plan: Fix Jenkins 30-min kill + live Dagger stdout streaming + live pipeline-view URL

Issue: https://github.com/disaster37/dagger-kubernetes/issues/19 — "fix: jenkins integration kill dagger after 30min. Not kill dagger on jenkins integration after 30min. Use jenkins timeout for that".
User requirements:
1. Dagger's stdout must be **streamed live** to the Jenkins console instead of buffered and printed only at the end.
2. As soon as Dagger starts and the trace ID is discovered, print the **full pipeline-view URL** (`<uiUrl>/pipelines/<traceID>`) to the live console so the user can open the pipeline view immediately — without waiting for the job to finish.

Branch: `fix/jenkins-timeout-live-streaming` (from `main`).

---

## 1. Summary

Two coordinated defects in the Jenkins integration:

1. **The 30-minute kill exists in two layers.** The `dagger-kubernetes-ci` wrapper defaults `--timeout` to `30m` and kills the `dagger` process via `context.WithTimeout`; the Jenkins shared library *also* wraps the run in `timeout(time: 30, unit: 'MINUTES')` by default. Neither is opt-in, so Dagger dies at 30 min regardless of what Jenkins is configured to do. The issue asks to remove the implicit kill and defer to Jenkins' native timeout.

2. **Dagger's stdout is not live.** In `--steps` mode the wrapper routes Dagger's stdout to its own stderr (ADR-024: stdout is the step-protocol channel). The Jenkins library redirects the wrapper's stderr to a temp file (`2>'${stderrFile}'`) and only `cat`s it in a `finally`, so Dagger's real output (and the trace-discovery line) is hidden until the command exits.

3. **The pipeline-view URL is only shown at the end.** The wrapper prints `Pipeline View: <url>` only during its final flush (after `dagger` exits); the library re-derives it in `finally`. The user must wait for the whole run to finish before they can open the pipeline view. The link must instead be emitted the moment the trace ID is discovered (~1s after Dagger starts).

Fix: make the wrapper's timeout opt-in (`0` = no timeout, negative rejected), remove the library's default `timeout()` wrapper, stop buffering stderr, hand the trace ID back to Jenkins through a side-channel file written as soon as it is discovered, and print the pipeline-view URL **live on discovery** (in addition to the end-of-run summary).

**Correction to the supplied reconnaissance:** the GHA (`ci-integrations/gha/dagger-kubernetes.sh:41`) and Drone (`ci-integrations/drone/config-extension.sh:65`) integrations do **not** invoke `dagger-kubernetes-ci` at all — they `exec dagger "$@"` directly. Therefore changing the wrapper's `--timeout` default affects only (a) Jenkins (which invokes the wrapper when present) and (b) direct `dagger-kubernetes-ci` users. GHA/Drone are unaffected by the wrapper default and rely on their own job timeouts today.

---

## 2. Root-cause analysis

### Layer A — Jenkins shared library (`ci-integrations/jenkins/vars/daggerKubernetes.groovy`)
- `:33` — `int timeoutMinutes = (params.timeoutMinutes ?: env.DAGGER_KUBERNETES_TIMEOUT_MINUTES ?: 30) as int` — defaults to 30.
- `:63` — `timeout(time: timeoutMinutes, unit: 'MINUTES') { ... }` — unconditional Jenkins-side kill.
- `:76–82` / `:89` — both the wrapper and fallback `sh` steps redirect stderr to `2>'${stderrFile}'` (`:56`), so nothing on stderr is live.
- `:94–105` — `finally` `cat`s the stderr file and `extractTraceId(stderr)` (`:175–183`) parses it for the trace ID, then `rm -f`s it.

### Layer B — CI wrapper (`cmd/ci/main.go`)
- `:90` — `&cli.DurationFlag{Name: "timeout", Value: 30 * time.Minute, ...}`.
- `:148–152` — `context.WithTimeout(...)` around `exec.CommandContext` — unconditional kill.
- `:157–161` — `--steps` mode: `cmd.Stdout = os.Stderr` (Dagger stdout → wrapper stderr, keeping stdout clean for the protocol).
- `:190` — `cmd.Stderr = io.MultiWriter(os.Stderr, &logBuf)` — stderr already streams from the wrapper's own perspective; the buffering happens *upstream in Jenkins*.
- `:229–257` — discovery goroutine polls `ListTraces(1)` every 1s and prints `[dagger-kubernetes-ci] discovered trace <id> from supervisor`; `:259–331` final flush prints `Pipeline View: <url>` — both currently only reach the console after the run, because Jenkins buffers stderr. The discovery line is the natural point to also emit the live URL.

---

## 3. Design decisions

### D1 — Wrapper `--timeout` becomes opt-in (`0` = no timeout)

- Change the `--timeout` default from `30 * time.Minute` to `0`.
- Semantics: `0` = no deadline (use `context.Background()`); `> 0` = `context.WithTimeout`; `< 0` = **reject** with `fmt.Errorf("--timeout must be >= 0")`.
- Update the diagnostic line (`:194–195`) to print `timeout=none` when `0`, else `timeout=<duration>`.
- **Rationale:** the issue is specifically "don't kill dagger after 30 min". A zero-default makes the wrapper non-destructive out of the box; an explicit positive value restores the kill for callers that want it. Rejecting negatives avoids a silently-inverted `WithTimeout(negative)` (which would create an already-expired context and kill instantly).
- **Why not keep `30m` and have Jenkins pass `--timeout 0`?** An *old* preinstalled wrapper interprets `--timeout 0` as `WithTimeout(0)` = already-expired → kills Dagger immediately. Passing `0` is therefore not version-skew-safe. Zero-default + Jenkins passing *nothing* is safe on both old and new wrappers (see D4).

### D2 — Jenkins library: no default `timeout()` wrapper

- Remove the unconditional `timeout(time: timeoutMinutes, unit: 'MINUTES')` wrapper.
- Wrap in Jenkins `timeout(...)` **only** when `timeoutMinutes` is explicitly set and `> 0`.
- `timeoutMinutes` absent / `0` / negative / non-numeric → **no wrapper**. Non-numeric values log a warning (`echo`) instead of throwing (Groovy `as int` on non-numeric currently crashes the build).
- **Do not forward** `timeoutMinutes` to the wrapper's `--timeout`. Jenkins `timeout()` is the single source of truth; forwarding would introduce double-kill races (Jenkins step vs wrapper context firing at ~the same instant).
- **Rationale:** "Use jenkins timeout for that." Document that users should set `timeoutMinutes: N` or Jenkins' native `options { timeout(time: N, unit: 'MINUTES') }` / a `timeout` step.

### D3 — Live streaming + trace-ID handoff via a side-channel file

- **Confirmed interpretation:** in `--steps` mode the wrapper's stdout is the plain/NDJSON step-protocol channel (ADR-024 §3); Dagger's stdout is deliberately written to the wrapper's stderr (`cmd/ci/main.go:157–158`). "Live" is achieved by **Jenkins no longer redirecting the wrapper's stderr to a file** — the wrapper's stderr (which carries Dagger stdout/stderr + the discovery line + the pipeline link) streams straight to the console.
- Add a `--trace-id-file` flag to the wrapper with `EnvVars: []string{"DAGGER_KUBERNETES_TRACE_ID_FILE"}`. The wrapper writes the discovered trace ID to that path.
- **Jenkins sets the env var, not the flag** (see D4), and stops redirecting stderr. In `finally`, Jenkins reads the file and prints the pipeline-view link; if the file is missing/empty it falls back to `/traces/latest`.
- **File format:** plain hex trace ID, **no trailing newline**, mode `0600`, written with a single `os.WriteFile`. Reader trims whitespace.
- **When written:** (1) immediately in the discovery goroutine after `discoveredID` is set; (2) idempotently at final flush with the resolved `traceID` (discovered or regex-fallback). Both writes are non-fatal (`logger.Debug` on error).
- **Atomicity:** direct write, **not** temp+rename. Rejected rationale: the only reader is Jenkins' `finally`, which runs after the wrapper process has exited; a truncated write (only possible on SIGKILL, and even then a 32–64-byte `write` is effectively atomic) yields an invalid ID → clean `/traces/latest` fallback.
- **Concurrency:** no extra mutex needed — `stepsWG.Wait()` (`:264`) joins the discovery goroutine before the final flush, so the two writes are sequential and idempotent.
- **Wrapper-absent fallback** (`dagger-kubernetes-ci` not on `PATH`): run `sh "${daggerCommand}"` live (no redirect); no trace discovery is possible → link `/traces/latest`.
- **Rejected alternative (c):** `tee`/process substitution (`2>&1 1> >(cat) | tee ...`) requires bash; Jenkins `sh` uses `/bin/sh` (dash) which has no process substitution. Rejected as unreliable.
- **Rejected alternative (a) flag-only:** Jenkins passing `--trace-id-file` would make an *old* preinstalled wrapper fail with `flag provided but not defined: -trace-id-file` (exit non-zero → build fails). The env-var form is backward compatible.

### D4 — Version skew (library vs preinstalled wrapper)

- Jenkins passes **only** the `DAGGER_KUBERNETES_TRACE_ID_FILE` env var and **no** `--timeout` flag.
- Old preinstalled wrapper: ignores the unknown env var (harmless), streams stderr live (Jenkins no longer redirects), never writes the file → Jenkins prints `/traces/latest`. Its own 30-min kill persists — an accepted, documented limitation.
- New wrapper (provisioned via `provisionCli`, or rebuilt): honors the env var → writes the file → exact `/pipelines/<id>` link; defaults to no timeout.
- The primary supported path (`provisionCli: true`, which prepends the freshly-downloaded wrapper to `PATH` in `provisionCli` `:275`) always gets the new binary. Document that preinstalled agent-image wrappers must be rebuilt to pick up the no-timeout default.

### D5 — Live pipeline-view URL emitted on discovery

- In the trace-discovery goroutine, immediately after `discoveredID` is set and the existing `[dagger-kubernetes-ci] discovered trace <id> from supervisor` line is printed, compute the URL with `domain.PipelineViewURL(uiURL, id)` and print it to stderr as `[dagger-kubernetes-ci] Pipeline View (live): <url>`. `uiURL` is already resolved in `run()` (`:119`) before the goroutine starts, so it is captured by the existing closure — **no signature change**; the goroutine gains `uiURL` as an additional captured local alongside `stepsSrc`, `stepsBuilder`, `stepsSink`, `pollInterval`, `steps`, `logger`.
- **`PipelineViewURL` error is non-fatal**: log at `logger.Debug` (with `trace_id` field) and skip the live line. The discovered trace ID comes from the supervisor and is already validated there; an error here means a bad base URL or an unexpected ID charset, neither of which should abort the build.
- **Deduplication:** keep **both** the live line and the existing end-of-run summary (`\nPipeline View: %s\n` at `:316`). The live line is clearly marked `(live)`; the final summary is always present even when discovery failed (it falls back to the regex-extracted or empty ID). Two `Pipeline View` lines per run are expected and documented.
- **Correct base URL is essential and requires a Jenkins-side change.** Jenkins currently invokes the wrapper with `--server '${serverUrl}' --config /dev/null` and no `--ui-url`. `config.Load("/dev/null")` returns compiled-in defaults, and `fileExists("/dev/null")` is true (`os.Stat` succeeds on the device node), so `resolveUIBase` picks the compiled-in default `server.public_url` (`https://supv.example.com`, `config/loader.go:34`) — **not** the Jenkins `serverUrl`/`uiUrl`. The live URL would therefore point at the wrong host. Fix: the library passes `--ui-url '${uiUrl}'` (after `assertShellSafe(uiUrl, 'uiUrl')`), making the wrapper's base deterministic and correct (see §4.2).
- **Rejected alternative — Jenkins background file-watcher:** a parallel `parallel`/background branch polling the trace-id file to echo the link early. Rejected: Jenkins `sh` steps are blocking and a watcher needs a second branch/background process that must be reliably killed and joined with the main run — fragile under CPS and the Jenkins timeout step. The wrapper-side live print is strictly simpler and streams with the existing stderr channel.

---

## 4. File-by-file change list

### 4.1 `cmd/ci/main.go`

| Location | Current | New |
|---|---|---|
| `ciFlags()` `:90` | `&cli.DurationFlag{Name: "timeout", Value: 30 * time.Minute, Usage: "maximum time the dagger command is allowed to run"}` | `Value: 0`, `Usage: "maximum time the dagger command is allowed to run (0 = no timeout)"` |
| `ciFlags()` (new flag) | — | `&cli.StringFlag{Name: "trace-id-file", EnvVars: []string{"DAGGER_KUBERNETES_TRACE_ID_FILE"}, Usage: "write the discovered trace ID to this file as soon as it is known (empty = disabled)"}` |
| `run()` `:148–152` | `timeout := c.Duration("timeout"); cmdCtx, cmdCancel := context.WithTimeout(...)` | validate + conditional (code shape below) |
| `run()` `:194–195` | `... timeout=%s\n", ..., timeout.String())` | `timeoutDisplay := "none"; if timeout > 0 { timeoutDisplay = timeout.String() }; ... timeout=%s\n", ..., timeoutDisplay)` |
| `run()` after flag reads | — | `traceIDFile := c.String("trace-id-file")` |
| discovery goroutine `:249` (after the `discovered trace` print) | — | emit the live URL + write the trace-id file (code shape below) |
| final flush after `:309` (`traceID` resolved) | — | `if traceID != "" { if werr := writeTraceIDFile(traceIDFile, traceID); werr != nil { logger.WithError(werr).Debug("write trace-id file failed") } }` |
| new helper (package-level, near `extractTraceID`) | — | `func writeTraceIDFile(path, id string) error` (below) |

Timeout/context code shape (illustrative — final code may vary, semantics fixed):

```go
timeout := c.Duration("timeout")
if timeout < 0 {
    return fmt.Errorf("--timeout must be >= 0")
}
var cmdCtx context.Context = context.Background()
var cmdCancel context.CancelFunc = func() {}
if timeout > 0 {
    cmdCtx, cmdCancel = context.WithTimeout(context.Background(), timeout)
}
defer cmdCancel()
//nolint:gosec // intentional: shell out to dagger CLI with user-supplied args
cmd := exec.CommandContext(cmdCtx, "dagger", cmdArgs...)
```

Helper:

```go
// writeTraceIDFile writes the discovered trace ID to path (plain hex, no
// trailing newline, 0600). Non-fatal by contract: CI integrations treat an
// absent/empty file as "no trace ID" and fall back to /traces/latest.
func writeTraceIDFile(path, id string) error {
    if path == "" || id == "" {
        return nil
    }
    return os.WriteFile(path, []byte(id), 0o600)
}
```

No new imports (`os`, `context`, `fmt` already imported). No config/domain changes — the timeout flag is not a viper key.

Discovery-goroutine code shape (insert inside the `if len(traces) > 0 && traces[0].TraceID != ""` branch, replacing the existing `discovered trace` print + `streamSteps` call):

```go
id := traces[0].TraceID
discoveredMu.Lock()
discoveredID = id
discoveredMu.Unlock()
fmt.Fprintf(os.Stderr, "[dagger-kubernetes-ci] discovered trace %s from supervisor\n", id)
// Live pipeline-view URL: emitted the moment the trace is known (~1s after
// dagger starts) so the user can open the pipeline view without waiting for
// the run to finish. Non-fatal on error.
if liveURL, uerr := domain.PipelineViewURL(uiURL, id); uerr != nil {
    logger.WithError(uerr).WithField("trace_id", id).Debug("live pipeline view url failed")
} else {
    fmt.Fprintf(os.Stderr, "[dagger-kubernetes-ci] Pipeline View (live): %s\n", liveURL)
}
if werr := writeTraceIDFile(traceIDFile, id); werr != nil {
    logger.WithError(werr).WithField("trace_id", id).Debug("write trace-id file failed")
}
if steps {
    streamSteps(ctx, stepsSrc, stepsBuilder, stepsSink, id, pollInterval, logger)
}
return
```

The end-of-run summary (`\nPipeline View: %s\n` at `:316`) is **kept unchanged** as the final authoritative link.

### 4.2 `ci-integrations/jenkins/vars/daggerKubernetes.groovy`

- `:33` — replace with `int timeoutMinutes = resolveTimeoutMinutes(params.timeoutMinutes ?: env.DAGGER_KUBERNETES_TIMEOUT_MINUTES)`.
- Add `resolveTimeoutMinutes(def)` helper (near `envTruthy`/`parseBool`):

```groovy
// resolveTimeoutMinutes returns a positive integer timeout in minutes, or 0
// (no timeout) when unset, zero, negative, or not a plain non-negative integer.
// A non-numeric value is a warning, not an error — Groovy `as int` would crash
// the build on a typo.
int resolveTimeoutMinutes(def value) {
    if (value == null || value == '') return 0
    String s = value.toString().trim()
    if (!(s ==~ /\d+/)) {
        echo "daggerKubernetes: ignoring non-numeric timeoutMinutes '${s}'"
        return 0
    }
    int m = s as int
    return m > 0 ? m : 0
}
```

- Replace the `stage("Dagger") { ... }` body (`:55–109`) with: build `traceIdFile`, `withEnv([...] + DAGGER_KUBERNETES_TRACE_ID_FILE + ...)` wrapping a conditional `timeout(...)`, delegating to a new `runDaggerCommand(...)` method. Code shape:

```groovy
stage("Dagger") {
    String traceIdFile = "/tmp/dagger-trace-${env.BUILD_NUMBER}.id"
    assertShellSafe(traceIdFile, 'trace-id file path')
    withEnv([
        "DAGGER_CLOUD_URL=${serverUrl}",
        "DAGGER_CLOUD_TOKEN=${token}",
        "DAGGER_KUBERNETES_TOKEN=${token}",
        "DAGGER_KUBERNETES_TRACE_ID_FILE=${traceIdFile}",
        "_EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self"
    ] + (version ? ["_EXPERIMENTAL_DAGGER_TAG=${version}"] : [])) {
        if (timeoutMinutes > 0) {
            timeout(time: timeoutMinutes, unit: 'MINUTES') {
                runDaggerCommand(daggerCommand, serverUrl, uiUrl, traceIdFile)
            }
        } else {
            runDaggerCommand(daggerCommand, serverUrl, uiUrl, traceIdFile)
        }
    }
}
```

New method (stderr no longer redirected — everything streams live; the wrapper prints the live URL on discovery, and the `finally` block reads the trace-id file for the end-of-stage link):

```groovy
// runDaggerCommand runs the dagger command with stderr/stdout streaming live to
// the console. The wrapper (when present) prints
// "[dagger-kubernetes-ci] Pipeline View (live): <url>" as soon as it discovers
// the trace ID, and writes the ID to traceIdFile (via
// DAGGER_KUBERNETES_TRACE_ID_FILE); the finally block reads it to print the
// end-of-stage link, falling back to /traces/latest. --ui-url is passed so the
// wrapper's links use the correct base (otherwise the wrapper falls back to the
// compiled-in server.public_url because --config /dev/null "exists").
void runDaggerCommand(String daggerCommand, String serverUrl, String uiUrl, String traceIdFile) {
    try {
        String ciBin = sh(script: "which dagger-kubernetes-ci 2>/dev/null || true", returnStdout: true).trim()
        if (ciBin) {
            String daggerArgs = daggerCommand.replaceFirst(/^dagger\s*/, '')
            assertShellSafe(serverUrl, 'serverUrl')
            assertShellSafe(uiUrl, 'uiUrl')
            sh """
                dagger-kubernetes-ci --steps --steps-format plain \
                    --server '${serverUrl}' \
                    --ui-url '${uiUrl}' \
                    --config /dev/null \
                    ${daggerArgs}
            """
        } else {
            sh daggerCommand
        }
    } catch (e) {
        echo "[dagger-kubernetes] Pipeline failed. View: ${uiUrl}/traces/latest"
        throw e
    } finally {
        String traceId = sh(script: "cat '${traceIdFile}' 2>/dev/null || true", returnStdout: true).trim()
        sh "rm -f '${traceIdFile}'"
        if (traceId) {
            echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/pipelines/${traceId}"
        } else {
            echo "[dagger-kubernetes] Pipeline View: ${uiUrl}/traces/latest"
        }
    }
}
```

Note: the wrapper now prints the live link itself (from its stderr), so the library's `finally` link is a **fallback/summary** — it covers old preinstalled wrappers (which neither print the live URL nor write the trace-id file → `/traces/latest`) and gives a clean end-of-stage link.

- **Remove `extractTraceId` (`:169–183`)** — it becomes dead: the `finally` no longer scans captured stderr (there is none), and the wrapper-absent path streams live with no capturable stderr. AGENTS.md dead-symbol discipline; grep the file to confirm no remaining references.

### 4.3 `cmd/ci/main_test.go`

New unit tests (reuse `newTestCLIContext`, `captureStderr`, `stubDaggerOnPath`; add small helpers below as needed). All table-driven where parameterized; `logrus` test logger already handled inside `run` via `observ`.

- `TestRunTimeoutNoneByDefault` — `stubDaggerOnPath(t, testTraceID)`; run without `--timeout`; assert `run(ctx)` returns nil and captured stderr contains `timeout=none`.
- `TestRunZeroTimeoutAccepted` — `--timeout 0`; assert success + `timeout=none`.
- `TestRunTimeoutKillsLongRunningCommand` — install a stub `dagger` that runs `sleep 5` (add a `stubDaggerSleeping(t, seconds)` helper mirroring `stubDaggerOnPath`); run with `--timeout 100ms`; assert `run` returns non-nil quickly (guard the whole call with `time.After(2s)`).
- `TestRunNegativeTimeoutRejected` — `--timeout -1s`; assert `run` returns an error containing `--timeout must be >= 0` and (via a marker-writing stub) that `dagger` was never executed.
- `TestTraceIDFileWrittenFromRegexFallback` — `stubDaggerOnPath(t, testTraceID)` + `--trace-id-file <t.TempDir()/id>`; assert file content equals `testTraceID` exactly (no trailing newline).
- `TestTraceIDFileNotWrittenWhenNoTraceID` — stub writes `"no trace id here"`; assert the file does not exist after `run`.
- `TestTraceIDFileEnvVar` — set `DAGGER_KUBERNETES_TRACE_ID_FILE=<tmp>` (no flag); assert file written (proves `EnvVars` sourcing).
- `TestTraceIDFileWriteErrorNonFatal` — `--trace-id-file <dir-that-does-not-exist>/x`; assert `run` returns nil and stderr still contains `Pipeline View:`.
- `TestTraceIDFileWrittenOnDiscovery` — `httptest.NewServer` serving `GET /api/v1/traces?limit=1` → `[{"trace_id":"<supervisor id>"}]` (the `domain.TraceListResult` shape; field `trace_id`); `--server <srv.URL>` + `--trace-id-file <tmp>`; stub `dagger` sleeps `2s` then exits; start `run(ctx)` in a goroutine, poll the file every 50ms, and assert (a) the file appears **before** `run` returns, (b) content == the supervisor id (not any dagger-stderr hex). This proves the discovery-time write path and its timing.
- `TestPipelineViewURLLiveOnDiscovery` — same httptest supervisor + sleeping dagger stub, with `--ui-url https://supv.example.com`; swap `os.Stderr` for a pipe's write end and run `run(ctx)` in a goroutine while reading the pipe concurrently (add a `captureStderrAsync` helper that returns a reader + a `done` channel, mirroring `captureStderr` but non-blocking). Assert (a) the line `[dagger-kubernetes-ci] Pipeline View (live): https://supv.example.com/pipelines/<supervisor id>` is read from the pipe **before** `run` returns (proves live, pre-exit emission), (b) it appears **exactly once**, and (c) after `run` returns, the full captured stderr contains the end-of-run summary `Pipeline View: https://supv.example.com/pipelines/<supervisor id>` (proves both lines coexist).
- `TestPipelineViewURLLiveErrorNonFatal` (optional but recommended) — httptest supervisor returns a trace with an invalid-ID charset so `PipelineViewURL` fails; assert `run` still returns nil, the `discovered trace` line is present, and no `Pipeline View (live):` line is emitted (fallback to the end-of-run summary).

### 4.4 `tests/integration/ci_steps_test.go`

Reuse `startCIStepsServer`, `buildCIWrapper`, `installFakeDagger`, `freeListener`. No hardcoded ports.

- `TestCIWrapperWritesTraceIDFile` — run the compiled wrapper with `--steps --steps-poll-interval 50ms --trace-id-file <tmp> --ui-url https://supv.example.com` against `startCIStepsServer`; assert the file contains `ciStepsTraceID` (the seeded supervisor trace), stdout still parses as NDJSON (`parseNDJSON`), stderr contains the **live** line `[dagger-kubernetes-ci] Pipeline View (live): https://supv.example.com/pipelines/<id>`, and stderr also contains the final `Pipeline View: https://supv.example.com/pipelines/<id>` summary.
- `TestCIWrapperStreamsDaggerStdoutToStderr` — install a fake `dagger` that prints a marker to **stdout** (`echo 'dagger-live-marker'`, no `>&2`) and `sleep 3` (new helper `installFakeDaggerWithStdout`); run with `--steps`; assert the marker appears in the wrapper's captured **stderr** and does **not** appear in stdout (stdout stays clean NDJSON). Proves the `cmd.Stdout = os.Stderr` live path.
- Existing `TestCIWrapperStreamsNestedSteps` and `TestCIWrapperPrintsSelfHostedURL` must continue to pass unchanged (no `--timeout` passed → default `0` → same non-kill behavior as today's `30m` default).

### 4.5 Docs

- `docs/README.md` §Jenkins:
  - `:1843–1849` — rewrite the "Dagger" stage bullet: stdout/stderr **stream live**; the wrapper prints `[dagger-kubernetes-ci] Pipeline View (live): <url>` as soon as it discovers the trace ID (~1s after Dagger starts) and writes the ID to the file named by `DAGGER_KUBERNETES_TRACE_ID_FILE`; the library reads that file in `finally` for the end-of-stage `/pipelines/<id>` link, falling back to `/traces/latest`. The library also passes `--ui-url '${uiUrl}'` so both links use the correct base.
  - `:1851` — replace "The whole run is wrapped in a configurable `timeout(...)` (default 30 minutes)." with "The run is **not** wrapped in a timeout by default. Set `timeoutMinutes: N` to add a Jenkins `timeout(...)` step, or use Jenkins' native `options { timeout(...) }`."
  - `:1853–1862` example — change `timeoutMinutes: 30)  // optional; default 30` to `timeoutMinutes: 30)  // optional; omit for no timeout`.
  - `:1870–1876` config-keys table — **no new key**; leave as-is (note in prose that `timeoutMinutes` is a library parameter, not a `ci.jenkins.*` config key).
- `config/config.app.yaml.sample` — **no change** (no new config key; `ci.jenkins` block `:325–328` is unchanged). Say so explicitly in the PR description.
- ADR — **new `docs/design/ADR-038-ci-timeout-live-streaming.md`** (ADR-037 is the current max). ADR-024 §3 (stdout = protocol, stderr = dagger output + link) remains true and is unchanged; its §4 is pre-existing staleness (the 2-stage rewrite), out of scope. New ADR records: wrapper `--timeout` zero-default + negative rejection; Jenkins opt-in `timeout()`; `DAGGER_KUBERNETES_TRACE_ID_FILE` handoff contract (format, timing, fallback); **live pipeline-view URL emission on discovery** (format `[dagger-kubernetes-ci] Pipeline View (live): <url>`, ~1s cadence, non-fatal on `PipelineViewURL` error, coexists with the end-of-run summary); the `--ui-url` base-URL correctness requirement; version-skew mitigation.
- `DAGGER.md` — **no change** (nothing touches `dagger/`, CI scripts, or `.github/workflows/`).

---

## 5. Edge cases & error handling

| Scenario | Behavior |
|---|---|
| `--timeout` omitted or `0` | No deadline; diagnostic `timeout=none` |
| `--timeout` negative (e.g. `-5m`) | `run` returns `--timeout must be >= 0`; dagger not executed |
| `--timeout` very small (e.g. `100ms`) | Context deadline kills `dagger`; `run` returns the exec error |
| Groovy `timeoutMinutes` absent / `0` / negative | No `timeout()` wrapper (0) |
| Groovy `timeoutMinutes` non-numeric (e.g. `"abc"`) | Warn via `echo`, treat as 0 (no wrapper) — no crash |
| Trace ID never discovered (supervisor unreachable, empty list) | File absent/empty → Jenkins prints `/traces/latest` |
| Wrapper SIGKILLed by an external timeout | No deferred write, but discovery-time write already persisted if discovery happened before the kill; else `/traces/latest` |
| Trace-id file path shell-unsafe | Jenkins `assertShellSafe(traceIdFile, ...)` fails the build; wrapper side is a Go path (no shell) |
| Concurrent discovery vs final flush | Sequential: `stepsWG.Wait()` joins the goroutine first; writes idempotent |
| `--steps` on/off | File written in both modes (discovery goroutine always runs; final flush always writes when `traceID != ""`) |
| plain vs ndjson | Independent of file writing |
| `os.WriteFile` failure (bad path, read-only fs) | Non-fatal (`logger.Debug`); link still printed to stderr; Jenkins falls back |
| Wrapper binary absent (`which` empty) | `sh daggerCommand` live; `/traces/latest` link at end only (no live URL — no discovery possible) |
| Live URL before discovery | Not possible: the URL is only emitted after the trace ID is known |
| `PipelineViewURL` error (bad base / invalid ID charset) | Non-fatal: `logger.Debug`, live line skipped, end-of-run summary still printed |
| Old preinstalled wrapper | No live URL line (old wrapper doesn't emit it) and no trace-id file → Jenkins prints `/traces/latest` at end only. Accepted limitation. |
| Duplicate URL lines (live + final) | Expected: `[dagger-kubernetes-ci] Pipeline View (live): …` on discovery + `Pipeline View: …` at final flush. Two lines per run are documented. |
| Wrong base URL (no `--ui-url`) | Without the §4.2 `--ui-url '${uiUrl}'` change, the wrapper resolves `uiURL` to the compiled-in default `https://supv.example.com` (`--config /dev/null` "exists") — live URL would be wrong. `--ui-url` + `assertShellSafe(uiUrl)` fixes it. |
| Windows / macOS | Out of scope (Jenkins `sh` assumes POSIX `cat`/`rm`, `/tmp`) |

---

## 6. Test plan

- **Unit** (`cmd/ci/main_test.go`): the eleven tests in §4.3 (`TestPipelineViewURLLiveErrorNonFatal` optional). Target: keep package coverage at ~100%; the new `writeTraceIDFile` helper is exercised for empty-path, write-success, and write-failure; the live-URL path is exercised for success (timing, once-only) and error (non-fatal).
- **Integration** (`tests/integration/ci_steps_test.go`): the two tests in §4.4 (with `TestCIWrapperWritesTraceIDFile` now asserting the live line too), plus re-run the existing `TestCIWrapperStreamsNestedSteps` and `TestCIWrapperPrintsSelfHostedURL`. Bind via `freeListener(t)`; timed shutdown in `t.Cleanup` (already the harness pattern).
- **Groovy:** there is **no automated harness** for the shared library. Best available verification:
  1. If Groovy is available locally: `groovy -c ci-integrations/jenkins/vars/daggerKubernetes.groovy` (syntax compile only — it does not execute Jenkins steps).
  2. Manual Jenkins smoke test (§7).
  3. The wrapper contract the Groovy depends on (trace-id file content/timing, live stderr) is covered by the integration tests above. This gap is acknowledged — the Groovy file itself has no CI coverage.

---

## 7. Verification & rollout

### 7.1 CI gate (mandatory)

```bash
dagger call -m ./dagger --src . ci export --path out
```

Minimum fallback when no Docker daemon:

```bash
go build ./... && go vet ./... && go test ./...
dagger call -m ./dagger --src . lint
```

golangci-lint `unused`: after editing `cmd/ci/main.go` and the Groovy file, grep touched symbols (`writeTraceIDFile`, `extractTraceId`) to confirm no dead code. Groovy is not linted by golangci-lint, so manually confirm `extractTraceId` is fully removed and unreferenced.

### 7.2 Local cluster redeploy (per `AGENTS.local.md` §4–§6)

The wrapper binary is baked into the supervisor image by the root `Dockerfile` (builds both `supervisor` and `dagger-kubernetes-ci`), so the Go-side fix reaches the cluster by redeploying the image. The Groovy library is **not** cluster-deployed (it is a Jenkins shared library consumed from the repo) — its validation is the manual smoke test below.

```bash
# 1. build (includes UI + both binaries)
docker build -t docker.io/disaster/dagger-kubernetes:dev .

# 2. push
docker push docker.io/disaster/dagger-kubernetes:dev

# 3. capture live values (mandatory — never upgrade without -f)
helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test \
  -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml

# 4. upgrade
helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test \
  ./deploy/helm/dagger-kubernetes \
  --namespace dagger-kubernetes-test \
  -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.image.tag=dev \
  --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes

# 5. force rollout (mutable dev tag + Always pull)
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s

# 6. agent verification (§5.1)
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test get pods \
  -l app.kubernetes.io/name=dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  port-forward svc/dagger-kubernetes-test-dagger-kubernetes-control 8080:80 &
curl -sk https://localhost:8080/healthz   # 200 "ok"
curl -sk https://localhost:8080/readyz    # 200
# + authed /api/v1/status, /api/v1/fleet, and log scan for panic/fatal/error
```

Documented fallback if Docker/`home` cluster unavailable: run the full unit + integration suites locally and note in the PR that the cluster redeploy (§7.2) and human UI verification (§5.2) are deferred — do **not** mark the work complete without them.

### 7.3 Manual Jenkins smoke test

1. `dynamicStages: true`, no `timeoutMinutes`, command `dagger core container from --address=alpine:3.20 with-exec --args="sh,-c,echo start-ok; sleep 5; echo end-ok" stdout`. Assert **`start-ok` appears in the console before the command finishes** (live streaming), **`[dagger-kubernetes-ci] Pipeline View (live): https://dagger.home.webcenter.fr/pipelines/<id>` appears within ~1s of start (before `end-ok`)** (live URL), and no 30-min kill.
2. Same but `timeoutMinutes: 1` with a command that sleeps `120s`: assert the Jenkins `timeout` step aborts at ~1 min (opt-in kill works) and the `finally` still prints the pipeline-view link.
3. Fallback: run with `provisionCli: false` on an agent where `dagger-kubernetes-ci` is absent — assert live output and `/traces/latest` (no live URL line, expected).
4. Regression: non-`dynamicStages` closure-body mode still works unchanged.

### 7.4 Branch / PR

```bash
git checkout -b fix/jenkins-timeout-live-streaming main
# ... commit changes ...
git push -u origin fix/jenkins-timeout-live-streaming
# open PR against main
```

---

## 8. Risks / open questions

- **Preinstalled old wrapper still kills at 30m** (its own default). Accepted limitation; documented; primary path (`provisionCli`) always ships the new binary.
- **Groovy library has no automated test** — the manual smoke test is the only execution proof. Acknowledged.
- **`extractTraceId` removal** changes the `/traces/latest` vs `/pipelines/<id>` behavior only in the wrapper-absent path (now always `/traces/latest`), which is already the documented behavior for that path.
- **Base-URL correctness depends on the `--ui-url '${uiUrl}'` change** (§4.2/D5). If omitted, the live and final wrapper links use the compiled-in default `server.public_url` (`https://supv.example.com`) because `--config /dev/null` "exists" — a wrong host. The Groovy change is therefore mandatory, not optional.
- **Two `Pipeline View` lines per run** (live + final) are intentional and documented; CI consumers that grep for the link should match the `(live)` variant or the final summary deliberately.
- **No open questions** remain for implementation; the design decisions above are final unless the reviewer disputes D1/D2/D3/D5.

## 9. Out of scope

- GHA / Drone integrations (they do not use the wrapper; unchanged).
- `--ci jenkins` annotation path (`emitJenkinsStages`) — unused by the library, unchanged.
- Supervisor-side changes (none needed; the fix is client-side).
- ADR-024 §4 staleness (the earlier 2-stage rewrite) — documented, not addressed here.
- Windows / macOS Jenkins agents.

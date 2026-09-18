# ADR-039: CI timeout is opt-in, Dagger output streams live, and the pipeline-view URL is emitted on discovery

- **Status:** accepted
- **Date:** 2026-09-18
- **Deciders:** dagger-kubernetes maintainers

## Context

Issue #19 ("jenkins integration kill dagger after 30min") reported that the
Jenkins integration kills Dagger after 30 minutes regardless of the Jenkins job
configuration, and that Dagger's output is not visible until the job ends.

Two coordinated defects caused this:

1. **The 30-minute kill existed in two layers.** The `dagger-kubernetes-ci`
   wrapper defaulted `--timeout` to `30m` and killed the `dagger` process via
   `context.WithTimeout`; the Jenkins shared library *also* wrapped the run in
   `timeout(time: 30, unit: 'MINUTES')` by default. Neither was opt-in.
2. **Dagger's stdout was not live.** In `--steps` mode the wrapper routes
   Dagger's stdout to its own stderr (ADR-024: stdout is the step-protocol
   channel). The Jenkins library redirected the wrapper's stderr to a temp file
   (`2>'${stderrFile}'`) and only `cat`ed it in a `finally`, so Dagger's real
   output — and the trace-discovery line — was hidden until the command exited.
3. **The pipeline-view URL was only shown at the end.** The wrapper printed
   `Pipeline View: <url>` only during its final flush (after `dagger` exited);
   the library re-derived it in `finally`. The user had to wait for the whole
   run before they could open the pipeline view.

## Decision

### 1. Wrapper `--timeout` is opt-in (`0` = no timeout)

- The `--timeout` default changes from `30m` to `0`.
- Semantics: `0` = no deadline (`context.Background()`); `> 0` =
  `context.WithTimeout`; `< 0` is rejected with `--timeout must be >= 0`.
- The diagnostic line prints `timeout=none` when `0`, else `timeout=<duration>`.

A zero default makes the wrapper non-destructive out of the box; an explicit
positive value restores the kill for callers that want it. Rejecting negatives
avoids a silently-inverted `WithTimeout(negative)` (an already-expired context
that would kill instantly).

Keeping `30m` and having Jenkins pass `--timeout 0` was rejected: an *old*
preinstalled wrapper interprets `--timeout 0` as `WithTimeout(0)` =
already-expired and kills Dagger immediately. A zero default plus Jenkins
passing *nothing* is safe on both old and new wrappers.

### 2. Jenkins library: no default `timeout()` wrapper

- The unconditional `timeout(time: timeoutMinutes, unit: 'MINUTES')` wrapper is
  removed. The run is wrapped only when `timeoutMinutes` is explicitly set and
  `> 0`.
- `timeoutMinutes` absent / `0` / negative / non-numeric → no wrapper.
  Non-numeric values log a warning (`echo`) instead of throwing (Groovy `as int`
  on non-numeric previously crashed the build).
- `timeoutMinutes` is **not** forwarded to the wrapper's `--timeout`: Jenkins
  `timeout()` is the single source of truth, and forwarding would introduce
  double-kill races.

Users who want a kill set `timeoutMinutes: N` or use Jenkins' native
`options { timeout(...) }` / a `timeout` step.

### 3. Live streaming + trace-ID handoff via a side-channel file

- In `--steps` mode the wrapper's stdout is the plain/NDJSON step-protocol
  channel (ADR-024 §3); Dagger's stdout is deliberately written to the wrapper's
  stderr. "Live" is achieved by **Jenkins no longer redirecting the wrapper's
  stderr to a file** — the wrapper's stderr (Dagger stdout/stderr + the
  discovery line + the pipeline link) streams straight to the console.
- A new `--trace-id-file` flag (env `DAGGER_KUBERNETES_TRACE_ID_FILE`) makes the
  wrapper write the discovered trace ID to that path.
- **Jenkins sets the env var, not the flag.** Passing `--trace-id-file` would
  make an *old* preinstalled wrapper fail with `flag provided but not defined`
  (exit non-zero → build fails); the env-var form is backward compatible.
- **File format:** plain trace ID, no trailing newline, mode `0600`, written
  with a single `os.WriteFile`. The reader trims whitespace.
- **Location:** the Jenkins library creates a private `mktemp -d` directory
  (mode `0700`) and points `DAGGER_KUBERNETES_TRACE_ID_FILE` at
  `<dir>/trace.id`. A predictable path in world-writable `/tmp` would let a
  local user on a shared agent pre-create or replace the file with a symlink,
  redirecting the wrapper's write (CWE-59/CWE-377) or making the `finally`
  `cat` echo an arbitrary file into the build log. The private directory is
  removed with `rm -rf` in `finally`.
- **When written:** (1) immediately in the discovery goroutine after
  `discoveredID` is set; (2) idempotently at final flush with the resolved
  `traceID` (discovered or regex fallback). Both writes are non-fatal
  (`logger.Debug` on error).
- **Atomicity:** direct write, not temp+rename. The only reader is Jenkins'
  `finally`, which runs after the wrapper process has exited; a truncated write
  (only possible on SIGKILL, and even then a 32–64-byte `write` is effectively
  atomic) yields an invalid ID → clean `/traces/latest` fallback.
- **Concurrency:** no extra mutex is needed — `stepsWG.Wait()` joins the
  discovery goroutine before the final flush, so the two writes are sequential
  and idempotent.
- **Wrapper-absent fallback** (`dagger-kubernetes-ci` not on `PATH`): run
  `sh "${daggerCommand}"` live (no redirect); no trace discovery is possible →
  link `/traces/latest`.

`tee`/process substitution was rejected because Jenkins `sh` uses `/bin/sh`
(dash), which has no process substitution.

### 4. Live pipeline-view URL emitted on discovery

- In the trace-discovery goroutine, immediately after `discoveredID` is set and
  the existing `[dagger-kubernetes-ci] discovered trace <id> from supervisor`
  line is printed, the wrapper computes `domain.PipelineViewURL(uiURL, id)` and
  prints `[dagger-kubernetes-ci] Pipeline View (live): <url>` to stderr.
- A `PipelineViewURL` error is **non-fatal**: log at `logger.Debug` (with a
  `trace_id` field) and skip the live line. The discovered ID comes from the
  supervisor and is already validated there; an error here means a bad base URL
  or an unexpected ID charset, neither of which should abort the build.
- **Deduplication:** both the live line and the existing end-of-run summary
  (`\nPipeline View: %s\n`) are kept. The live line is marked `(live)`; the
  final summary is always present even when discovery failed. Two
  `Pipeline View` lines per run are expected and documented.

A Jenkins background file-watcher was rejected: Jenkins `sh` steps are blocking
and a watcher needs a second branch/background process that must be reliably
killed and joined with the main run — fragile under CPS and the Jenkins timeout
step. The wrapper-side live print is strictly simpler and streams with the
existing stderr channel.

### 5. `--ui-url` base-URL correctness

Jenkins previously invoked the wrapper with `--server '${serverUrl}' --config
/dev/null` and no `--ui-url`. `config.Load("/dev/null")` returns compiled-in
defaults, and `fileExists("/dev/null")` is true (`os.Stat` succeeds on the
device node), so `resolveUIBase` picked the compiled-in default
`server.public_url` — not the Jenkins `serverUrl`/`uiUrl`. The live URL would
therefore point at the wrong host. The library now passes `--ui-url '${uiUrl}'`
(after `assertShellSafe(uiUrl, 'uiUrl')`), making the wrapper's base
deterministic and correct.

### 6. Version skew (library vs preinstalled wrapper)

- Jenkins passes **only** the `DAGGER_KUBERNETES_TRACE_ID_FILE` env var and
  **no** `--timeout` flag.
- Old preinstalled wrapper: ignores the unknown env var (harmless), streams
  stderr live (Jenkins no longer redirects), never writes the file → Jenkins
  prints `/traces/latest`. Its own 30-minute kill persists — an accepted,
  documented limitation.
- New wrapper (provisioned via `provisionCli`, or rebuilt): honors the env var →
  writes the file → exact `/pipelines/<id>` link; defaults to no timeout.
- The primary supported path (`provisionCli: true`) always gets the new binary.
  Preinstalled agent-image wrappers must be rebuilt to pick up the no-timeout
  default.

## Consequences

- Dagger no longer dies at 30 minutes unless the user opts in; Jenkins' own
  timeout is the single source of truth.
- Dagger's stdout/stderr and the pipeline-view URL are visible live in the
  Jenkins console.
- The trace-ID file is a small, backward-compatible side channel; an absent or
  empty file degrades cleanly to `/traces/latest`.
- Two `Pipeline View` lines per run are intentional; CI consumers that grep for
  the link should match the `(live)` variant or the final summary deliberately.
- The Groovy shared library has no automated test harness; the wrapper contract
  it depends on (trace-ID file content/timing, live stderr) is covered by the
  integration tests, and the library itself is validated by a manual Jenkins
  smoke test.

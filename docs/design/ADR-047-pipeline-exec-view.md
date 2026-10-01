# ADR-047: Surface executed commands (exec spans) in the pipeline view

**Status:** Accepted  
**Date:** 2026-10-01

## Context

The pipeline view showed a step's name, status, duration and logs, but never
the **command that was executed** — argv, working directory, environment, exit
code, or operation kind. For a `dagger build`/`publish`, the user could not see
the image push/exec command (issue #60).

The exec data already rides the OTLP spans the supervisor ingests, but it was
discarded at two layers:

1. **Ingest dropped the value types.** `mapToSpanNode`
   (`internal/repository/trace_store.go`) preserved only `stringValue` and
   `boolValue` OTLP attribute values. Dagger emits exec argv and env as
   **string arrays** (`arrayValue`) and the exit code as an **int**
   (`intValue`) / a span **event** (`exit.code`); all were silently dropped.
2. **No exec view-model.** Even the surviving string attributes were not
   interpreted as an "exec command"; the UI rendered only name/status/duration
   plus logs.

## Decision

### D1 — Preserve all OTLP value types + span events at ingest

`mapToSpanNode` now flattens every OTLP `AnyValue` to a string via
`flattenAttrValue`: string, bool, int (protojson string or JSON number),
double, and **string arrays encoded as a JSON array string** (e.g.
`["go","build"]`). `SpanNode.Attributes` stays `map[string]string`, so every
existing consumer (search, CI steps, UI) keeps working unchanged. A new
`parseSpanEvents` decodes the OTLP `events` array into `SpanNode.Events`
(`[]domain.SpanEvent`), carrying the `Container exited` / `exit.code` event.

The JSON-array flattening rule is lossless and unambiguous; the service layer
decodes it. Malformed values/events are skipped, never panicking.

### D2 — Derive the exec view-model server-side, in the service layer

Per the dependency rule (`handler → service → domain ← repository`), the
mapping from raw span attributes to a typed `ExecInfo` is business logic and
lives in `internal/service/exec_info.go`. `repository` stays a faithful
OTLP→`SpanNode` decoder; `handler.handleTracesDetail` calls
`service.DeriveExec(trace.RootSpan)` before writing JSON. `DeriveExec` is a
pure transform over the freshly-built per-request tree (no shared state, no
locking).

`execFromSpan` returns `nil` unless at least one of argv, cwd, env, exit code,
or an exec/publish-like span name is present, so `exec` stays off non-exec
spans (smaller payload, no UI noise).

### D3 — Attribute-key priority table

The Dagger/OTel keys are centralized in one Go table (`execAttrKeys`), first
present wins, so a future engine rename is a one-line change:

| Fact | Keys (priority order) |
|---|---|
| argv / command | `dagger.io/exec.args`, `process.command_args`, `wcprof.exec.argv`, `dagger.io/exec.argv`; then `dagger.io/dag.call` (see D3a) |
| working dir | `dagger.io/exec.cwd`, `process.cwd` |
| user | `dagger.io/exec.user`, `process.owner` |
| env (non-secret) | `dagger.io/exec.env`, `process.environment_variable.<name>` |
| secret env names | `dagger.io/exec.secret.env` |
| exit code | span event `Container exited` → `exit.code`; then `dagger.io/exec.exitCode`, `exit.code` |
| operation kind | `dagger.io/cache.type`, `wcprof.op.kind`; else derived from the span name |

### D3a — Verified reality: argv arrives in `dagger.io/dag.call`

A live trace from engine **v0.21.8** showed that the argv keys above are **not
emitted**. The only carrier of the operation and its arguments is the span
attribute **`dagger.io/dag.call`**: a base64-encoded `callpbv1.Call` protobuf
(`dagger/dagger` `dagql/call/callpbv1`). For example a `Container.withExec`
span decodes to field `withExec` with `args = ["sh","-c","echo …; exit 0"]`.

`internal/service/dag_call.go` is a hand-rolled, panic-free protobuf wire
parser (stdlib only) for that message. Field numbers:

| Message | Field | Number | Type |
|---|---|---|---|
| `Call` | `field` | 3 | string |
| `Call` | `args` | 4 | repeated `Argument` |
| `Argument` | `name` | 1 | string |
| `Argument` | `value` | 2 | `Literal` |
| `Literal` | `string` | 7 | string |
| `Literal` | `list` | 8 | `List` |
| `List` | `values` | 1 | repeated `Literal` |

`parseArgv` falls back to `parseDagCall` after the dedicated argv keys: when the
call field is `withExec`/`exec` (case-insensitive) and the `args` argument is
non-empty, that argv is used. The dedicated keys keep precedence, so a future
engine that emits them still wins.

For io operations (`publish`, `export`, `import`, `push`) the target rides an
`address` argument. When no argv was found, `execFromSpan` synthesizes a command
line `append([]string{field}, args["address"]...)` — e.g.
`publish ghcr.io/org/image:tag` — and keeps `Kind = io` from
`classifyExecKind`. When the address is absent the argv is just the field name;
nothing is fabricated.

The exit code is unchanged: on v0.21.8 it rides the child `resume withExec`
span's `Container exited` event (`exit.code`), which D1 already preserves.

### D3b — Verified reality: env vars arrive in `dagger.io/dag.call`

The same live trace showed that `Container.withEnvVariable` and
`Container.withSecretVariable` spans carry **no** `dagger.io/exec.env` /
`process.environment_variable.*` attributes; `dagger.io/dag.call` is the only
signal. `callEnv` decodes it and derives the env entry:

| Call field (case-insensitive) | Derived entry |
|---|---|
| `withEnvVariable`, `withEnvironmentVariable` | `{name, value}` from the `name`/`value` arguments, via `envVar` (secret-name redaction + value cap) |
| `withSecretVariable` | `{name, is_secret: true}` from the `name` argument; the `secret` argument is a `Literal.id` and is **never** surfaced |

`execFromSpan` tries `parseEnv` first (dedicated attributes keep precedence)
and falls back to `callEnv`; when the call yields entries and the span kind was
`other`, the kind is set to `env`. A span with only a `withEnvVariable` call
therefore renders as an env operation rather than a bare `other` span.

### D4 — Secret redaction

Secret env names come from `dagger.io/exec.secret.env` (or a
`withSecretVariable` call, D3b); their values are never present in telemetry,
so they render as `is_secret: true` with an empty value (the UI shows
`<secret>`). As defense-in-depth, any non-secret value whose
**name** matches a secret-ish pattern (`*TOKEN*`, `*SECRET*`, `*PASSWORD*`,
`*KEY*`, `*AUTH*`, case-insensitive) is redacted to `<redacted>`. The raw
`Attributes` map is unchanged (already bounded by the OTLP body cap).

### D5 — Size caps

`ExecInfo` derivation caps each value at 4 KiB, env entries at 200, and argv at
256 elements, truncating with a trailing `…` marker. This bounds the response
size (CWE-400) without hiding the command.

### D6 — No new endpoint and no new SSE event

The existing `GET /api/v1/traces/:traceID` response gains optional `events` and
`exec` fields on each `SpanNode` (both `omitempty`). The pipeline view already
re-fetches the trace on the `trace_update` SSE event and on the 5s poll, so
exec data refreshes for free. The change is purely additive and backward
compatible: older clients ignore unknown fields, and the CI event stream
(`ci_steps.go`) reads only `Attributes`/`Name`/status.

## Alternatives considered

- **A dedicated `GET /api/v1/traces/:id/exec` endpoint** — rejected: the data
  is already part of the span tree; a second endpoint adds auth/scoping surface
  and a second fetch for no gain.
- **A new SSE event type** — rejected: the existing `trace_update` already
  triggers a re-fetch; a new event would duplicate the refresh path.
- **Derive exec in the UI from raw attributes** — rejected: the attribute-key
  priority/redaction/caps are business rules that belong server-side, and the
  UI would need the same table duplicated.
- **Redact secret-ish values in the raw `Attributes` map at ingest** —
  rejected: ingest is a faithful decoder and does not know which keys are
  secret; redaction is a view-model concern.

## Consequences

- **No config change.** Exec derivation is always-on and bounded by hardcoded
  caps; no new `config/loader.go` key or `config/config.app.yaml.sample` entry.
- **API:** `GET /api/v1/traces/:traceID` `root_span` nodes may now include
  `events` and `exec`; no removed/renamed fields. Auth/scoping unchanged.
- **UI:** the focused-step header renders the command line (click to copy),
  cwd/user/exit-code/kind badges, and a collapsible environment list; secret
  entries show `<secret>`, redacted entries `<redacted>`. Rendered via `{{ }}`
  interpolation only (no `v-html`), per ADR-038.
- **Risk — attribute-key drift:** the exact keys vary across Dagger engine
  versions. The priority table plus tolerant parsing mitigates this; adding a
  key is a one-line change with no data-model change. The v0.21.8 live trace
  resolved the main open question: argv rides `dagger.io/dag.call` (D3a), not
  the documented argv keys.
- **Risk — image "push" visibility:** a `Container.publish`/`Export` may not
  emit a classic exec span. The operation span's name plus the synthesized
  `publish <address>` command line (D3a) surface the row; a dedicated publish
  view is a follow-up, not #60.

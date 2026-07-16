# Compliance Audit — Implementation Plan

## Decision: Update docs to match codebase; fix only small violations

**Rationale:** The entire project is built around `net/http`, `zap`, `flag`, and `gorilla/websocket`. Refactoring 25+ files to urfave/cli, hertz, and logrus would provide zero functional value and introduce risk. The codebase follows good patterns (DI, viper config, import ordering, testing). The pragmatic approach is to align AGENTS.md/CONTRIBUTING.md with reality, then fix the genuine bugs (missing `%w` wrapping, string `+` concat, `"log"` package).

---

## Step 1 — Update AGENTS.md and CONTRIBUTING.md to reflect actual libraries

Files to modify:
- `AGENTS.md`
- `CONTRIBUTING.md`

Changes:
- `CLI` → `flag` (stdlib)
- `HTTP` → `net/http` (stdlib)
- `Logging` → `go.uber.org/zap`
- `WebSocket` → `github.com/gorilla/websocket`
- Remove "CloudWeGo libs" preference lines
- Update all code examples (logrus → zap, hertz → net/http, urfave/cli → flag)
- Update project structure comments (remove urfave/cli, hertz, logrus references)
- Update PR checklist items

Dependencies: None (first step).
Verification: `grep -c "urfave/cli" AGENTS.md` → 0, `grep -c "hertz" AGENTS.md` → 0, `grep -c "logrus" AGENTS.md` → 0.

---

## Step 2 — Create ADR documenting the compliance decision

File to create: `docs/design/adr-002-library-choices.md`

Content: Record that the project evaluated AGENTS.md library mandates against actual implementation and chose to align docs with code rather than refactor. State the actual library stack and rationale.

Dependencies: Step 1 (so ADR matches the updated docs).
Verification: File exists at `docs/design/adr-002-library-choices.md`.

---

## Step 3 — Fix string concatenation violations

Files to modify:
- `internal/telemetry/traces.go:54`

Change:
- `r.httpClient.Get(r.tempoURL + "/api/traces/" + traceID)` → `r.httpClient.Get(fmt.Sprintf("%s/api/traces/%s", r.tempoURL, traceID))`

Dependencies: None.
Verification: `grep -n '"+' internal/telemetry/traces.go` should show no lines with `+` used for string concatenation with string literals (only error wrapping or type conversions should remain).

---

## Step 4 — Fix error wrapping violations (23 occurrences)

Files to modify:
- `internal/auth/token.go:30,39,76,88`
- `internal/api/server.go:291`
- `cmd/supervisor/main.go:156`
- `internal/telemetry/logs.go:33,58`
- `internal/telemetry/metrics.go:33,46,68`
- `internal/fleet/stub.go:92,119,134,139`
- `internal/session/store.go:55,58,69,81,93`
- `internal/ca/ca.go:76,86,181`

Changes:
- Lines returning `fmt.Errorf("message")` without wrapping an `err` → wrapped with `%w` if `err` is a variable in scope
- Lines returning bare `err` → wrapped with context via `fmt.Errorf("context: %w", err)`
- Lines returning `err` directly from `os.ReadFile` or similar I/O calls → wrap with `fmt.Errorf("read tokens file: %w", err)`

Specific cases:
- `internal/auth/token.go:30`: `"empty token"` — no `err` to wrap, keep as-is (not a violation)
- `internal/auth/token.go:39`: same, keep as-is
- `internal/auth/token.go:76`: `"missing authorization"` — same, keep
- `internal/auth/token.go:88`: `"unsupported auth scheme"` — same, keep
- Lines that return `err` directly without wrapping: wrap each with `fmt.Errorf("context: %w", err)`

Dependencies: None (can run in parallel with Step 3).
Verification: `grep -n 'return.*err'` on each file checked; verify `%w` is present where a non-nil `err` is returned.

---

## Step 5 — Replace `"log"` package with zap logging

Files to modify:
- `internal/auth/token.go`

Changes:
- Remove `"log"` import
- Add zap logger to `TokenValidator` struct (constructor injection)
- Replace `log.Printf(...)` call in `checkTokenFile` with `v.logger.Warn(...)`
- Update `NewTokenValidator` signature to accept `*zap.Logger`
- Update all callers of `NewTokenValidator` to pass a logger

Files that call `NewTokenValidator` (need update):
- `internal/api/server.go` — inject `zap.Logger` into `NewTokenValidator`
- `cmd/supervisor/main.go` — inject `zap.Logger` into `NewTokenValidator`
- `test/integration_test.go` — inject `zap.NewNop()` into `NewTokenValidator`

Dependencies: Step 1 (so docs reflect zap), but code-wise can proceed independently.
Verification: `grep -rn '"log"' internal/auth/token.go` → empty; `go build ./...` passes.

---

## Priority ordering (execution order)

1. **Step 1** — Update AGENTS.md + CONTRIBUTING.md (foundation, unblocks everything else)
2. **Step 2** — Create ADR (documents the decision, references updated AGENTS.md)
3. **Step 3** — Fix string concatenation (trivial, no deps)
4. **Step 4** — Fix error wrapping (independent of Step 3)
5. **Step 5** — Replace `"log"` package (independent of Steps 3-4)

Steps 3, 4, and 5 can be done in parallel.

---

## Final verification

```bash
# Build
go build ./...

# Tests
go test -race -coverprofile=coverage.out -covermode=atomic ./...

# Lint
golangci-lint run ./...

# Verify no remaining violations of fixed categories
grep -rn 'log.Printf' internal/   # should only find this plan, not code
grep -rn '"github.com/sirupsen/logrus"' go.mod  # empty (not a dependency)
grep -rn '"github.com/cloudwego/hertz"' go.mod  # empty (not a dependency)
grep -rn '"github.com/urfave/cli/v3"' go.mod   # empty (not a dependency)
```

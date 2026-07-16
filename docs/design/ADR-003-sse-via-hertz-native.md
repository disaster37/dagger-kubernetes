# ADR-003: SSE via Hertz Native

**Status:** Accepted · **Date:** 2025-07-15 · **Author:** compliance team

## Context

The live trace streaming endpoint (`GET /api/v1/traces/:traceID/live`)
pushed span updates to connected clients. The original implementation used
`gorilla/websocket`. AGENTS.md mandates Hertz's native `pkg/protocol/sse`
for server-to-client push and disallows `gorilla/websocket`.

## Decision

Replace `gorilla/websocket` with Hertz's built-in SSE writer:

```go
import "github.com/cloudwego/hertz/pkg/protocol/sse"

w := sse.NewWriter(c)
defer w.Close()

w.WriteEvent("message", []byte(data))
w.WriteKeepAlive()  // 30-second heartbeat
```

The `LiveClient` struct wraps the `*sse.Writer` instead of a WebSocket
connection. The `LiveHub` manages subscriptions and broadcasts via the
client's `Send` channel, with `writePump` draining events to the SSE
writer.

## Rationale

- Hertz SSE is a built-in, zero-dependency feature.
- SSE is simpler than WebSocket for unidirectional server→client push.
- No external dependency on `gorilla/websocket` or `hertz-contrib/websocket`.

## Consequences

- The `internal/telemetry/live.go` `LiveClient.Conn` field is replaced with
  an SSE writer abstraction.
- The `handleTracesLive` handler sets standard SSE headers
  (`Content-Type: text/event-stream`, `Cache-Control: no-cache`,
  `Connection: keep-alive`) before creating the client.
- The `writePump` exits on context cancellation, cleanly closing the
  response writer.

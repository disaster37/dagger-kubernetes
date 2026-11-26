# ADR-054: Framed binary otlpstream read-back

## Status
Accepted

## Context
`dagger trace` and `dagger agent -r` read traces/logs/metrics back from Cloud
using a framed binary stream.

## Decision
- Define the 16-byte `otlpstream` frame header (`DTP\x01` data/heartbeat,
  `DTE\x01` terminal, `DTX\x01` error) with `Content-Type:
  application/vnd.dagger.otlp.stream`.
- Tee raw OTLP payloads into a per-trace live buffer and micro-batch them to S3
  (`otlp-archive/<traceID>/<signal>/<seq>.bin`, 4 MiB / 2s).
- Serve `GET /v1/{traces,logs,metrics}/:traceID`, with 15s heartbeats and a
  terminal frame once the trace completes.
- Pin these GETs to the leader and enable `ImmediateHeaderFlush` so
  `httpext.Forwarder` streams frames without buffering.
- Delete archived chunks from `HistoryService` purge/GC.

## Consequences
Read-back is lossless and streaming, and history retention covers archives.

## Update — residual-risk remediation (PR #70 review)
- Sequence numbers are assigned exactly once at ingest and are immutable.
  Archive retries reuse the same `(Seq, Payload)` pair, eliminating drift.
- `OTLPArchive.ChunksAfter(fromSeq)` uses S3 `ListObjectsOptions.StartAfter`
  so cold reads are `O(k)` in newly archived chunks, never `O(n)`.
- `GET /v1/{traces,logs,metrics}/:traceID?after=<seq>` resumes at `seq+1`.
  Live readers wake on an in-memory notify channel (1s fallback poll) and the
  terminal frame is emitted only after a final drain.
- The live ring is bounded (256 chunks / 16 MiB) and idle terminal buffers are
  evicted after 30s.

import type { LogSearchMode, SpanNode } from '@/api/types'

// Internal-span name rules, ported from internal/service/ci_steps.go
// (internalSpanPrefixes/internalSpanExact) so the pipeline UI folds the same
// engine transport spans the CI step builder does. Kept in sync manually.
export const INTERNAL_SPAN_PREFIXES = [
  'GET ', 'POST ', 'PUT ', 'DELETE ', 'PATCH ', 'HEAD ', 'OPTIONS ',
  'Read ', 'Write ', 'Query.', 'Address.', 'parsing ',
] as const
export const INTERNAL_SPAN_EXACT = new Set<string>(['connect'])

export function isInternalSpanName(name: string): boolean {
  return INTERNAL_SPAN_PREFIXES.some((p) => name.startsWith(p)) || INTERNAL_SPAN_EXACT.has(name)
}

export function attrBool(n: SpanNode, key: string): boolean {
  return n.attributes?.[key] === 'true'
}

// A span is internal noise (its logs are dropped) when Dagger marks it
// internal or its name matches the CI internal-span rules.
export function isInternalSpan(n: SpanNode): boolean {
  return attrBool(n, 'dagger.io/ui.internal') || isInternalSpanName(n.name)
}

// A span is hidden from the tree when it is internal noise or encapsulated.
export function isHiddenSpan(n: SpanNode): boolean {
  return isInternalSpan(n) || attrBool(n, 'dagger.io/ui.encapsulated')
}

// A transparent span is hidden but its logs belong to the nearest visible
// ancestor (passthrough promotes children; encapsulated hides the node).
export function isTransparentSpan(n: SpanNode): boolean {
  return attrBool(n, 'dagger.io/ui.passthrough') || attrBool(n, 'dagger.io/ui.encapsulated')
}

export interface DisplaySpan {
  node: SpanNode
  depth: number
}

// visibleChildren returns the direct children of nodes after passthrough
// promotion and internal-span folding (port of the old topLevelSpans).
export function visibleChildren(nodes: SpanNode[]): SpanNode[] {
  const out: SpanNode[] = []
  for (const n of nodes) {
    if (attrBool(n, 'dagger.io/ui.passthrough') || isInternalSpan(n)) {
      out.push(...visibleChildren(n.children))
    } else {
      out.push(n)
    }
  }
  return out
}

// flattenVisible walks a node's subtree, promoting passthrough spans and
// hiding internal/encapsulated spans (counting the latter as hidden).
export function flattenVisible(node: SpanNode, depth: number): { spans: DisplaySpan[]; hidden: number } {
  if (attrBool(node, 'dagger.io/ui.passthrough')) {
    const result = { spans: [] as DisplaySpan[], hidden: 0 }
    for (const child of node.children) {
      const r = flattenVisible(child, depth)
      result.spans.push(...r.spans)
      result.hidden += r.hidden
    }
    return result
  }
  if (isHiddenSpan(node)) {
    const result = { spans: [] as DisplaySpan[], hidden: 1 }
    for (const child of node.children) {
      const r = flattenVisible(child, depth)
      result.spans.push(...r.spans)
      result.hidden += r.hidden
    }
    return result
  }
  const result = { spans: [{ node, depth } as DisplaySpan], hidden: 0 }
  for (const child of node.children) {
    const r = flattenVisible(child, depth + 1)
    result.spans.push(...r.spans)
    result.hidden += r.hidden
  }
  return result
}

// --- Duration helpers -----------------------------------------------------

export function spanStartMs(n: SpanNode): number {
  const t = Date.parse(n.start_time)
  return Number.isNaN(t) ? 0 : t
}

export function spanEndMs(n: SpanNode): number {
  const start = spanStartMs(n)
  const duration = n.duration_ms || (n.duration_ns ? n.duration_ns / 1e6 : 0)
  return start ? start + duration : 0
}

// subtreeDuration measures the wall-clock time spanned by a node and all of
// its descendants (some Dagger spans, e.g. "connect", have no end time but
// their children do).
export function subtreeDuration(node: SpanNode): number {
  let minStart = Infinity
  let maxEnd = 0
  const walk = (n: SpanNode) => {
    const s = spanStartMs(n)
    if (s) minStart = Math.min(minStart, s)
    const e = spanEndMs(n)
    if (e) maxEnd = Math.max(maxEnd, e)
    for (const c of n.children) walk(c)
  }
  walk(node)
  if (minStart === Infinity || maxEnd <= minStart) return node.duration_ms || 0
  return Math.round(maxEnd - minStart)
}

// liveSpanDuration ticks upward for a running span (now − start_time) and
// freezes at the stored subtree wall-clock once finished. Returns ms.
export function liveSpanDuration(node: SpanNode, now: number): number {
  if (node.status !== 'running') return subtreeDuration(node)
  const start = spanStartMs(node)
  if (!start) return node.duration_ms || 0
  return Math.max(0, now - start)
}

export function formatDuration(ms: number | null | undefined): string {
  if (!ms || ms <= 0) return '-'
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  return `${m}m ${(s % 60).toFixed(0)}s`
}

// --- Log text rendering ---------------------------------------------------

interface LogJSON {
  body?: unknown
  attributes?: { stdio?: { stream?: number; eof?: boolean } }
}

// Loki stores each log record as a JSON object; extract the human-readable
// `body` field when present, strip ANSI colour escapes and a leading
// "Stdout:"/"Stderr:" stream prefix, and keep the payload. Returns null only
// for empty/whitespace-only records (including stdio.eof markers) so the
// renderer can skip them without hiding content-bearing exec/RUN output.
export function logText(line: string): string | null {
  let text = line
  try {
    const obj = JSON.parse(line) as LogJSON
    if (obj && typeof obj.body === 'string') text = obj.body
  } catch {
    // not JSON; render the raw line
  }

  text = text.replace(/\u001b\[[0-9;]*m/g, '')
  text = text.replace(/^(Stdout|Stderr):\s*\n?/, '')

  if (text.trim() === '') return null

  text = text.replace(/\n+$/, '')

  // The Dagger engine serialises verbose progress payloads (module schemas,
  // telemetry graphs) as JSON-quoted base64 protobufs in the log body. Those
  // are binary, not human-readable, so decode base64 that is valid UTF-8 text
  // and collapse binary payloads to a placeholder instead of rendering base64.
  let candidate = text.trim()
  if (candidate.length > 2 && candidate.startsWith('"') && candidate.endsWith('"')) {
    try {
      const inner = JSON.parse(candidate)
      if (typeof inner === 'string') candidate = inner
    } catch {
      // keep the quoted candidate as-is
    }
  }
  if (isBase64(candidate)) {
    const decoded = decodeBase64UTF8(candidate)
    return decoded !== null ? decoded : '[binary log data]'
  }
  return text
}

function isBase64(s: string): boolean {
  if (s.length < 8 || s.length % 4 !== 0) return false
  return /^[A-Za-z0-9+/]+={0,2}$/.test(s)
}

function decodeBase64UTF8(s: string): string | null {
  try {
    const bin = atob(s)
    const bytes = new Uint8Array(bin.length)
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return null
  }
}

export interface HighlightSegment {
  text: string
  match: boolean
}

// highlightSegments splits text into alternating non-match/match segments for
// the given query. Contains mode is a case-sensitive literal split; regex mode
// uses the (already server-validated) pattern. Returns a single non-match
// segment when the query is empty or does not match. Never returns HTML: the
// caller renders segments with interpolation + <mark> (no v-html).
export function highlightSegments(text: string, query: string, mode: LogSearchMode): HighlightSegment[] {
  if (!query) return [{ text, match: false }]

  if (mode === 'regex') {
    let re: RegExp
    try {
      re = new RegExp(query, 'g')
    } catch {
      return [{ text, match: false }]
    }
    return splitByRegex(text, re)
  }

  const segments: HighlightSegment[] = []
  let index = 0
  while (index < text.length) {
    const found = text.indexOf(query, index)
    if (found === -1) {
      segments.push({ text: text.slice(index), match: false })
      break
    }
    if (found > index) segments.push({ text: text.slice(index, found), match: false })
    segments.push({ text: text.slice(found, found + query.length), match: true })
    index = found + query.length
  }
  return segments.length > 0 ? segments : [{ text, match: false }]
}

function splitByRegex(text: string, re: RegExp): HighlightSegment[] {
  const segments: HighlightSegment[] = []
  let last = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(text)) !== null) {
    if (m[0] === '') {
      // Zero-width match: advance to avoid an infinite loop.
      re.lastIndex++
      continue
    }
    if (m.index > last) segments.push({ text: text.slice(last, m.index), match: false })
    segments.push({ text: m[0], match: true })
    last = m.index + m[0].length
  }
  if (last < text.length) segments.push({ text: text.slice(last), match: false })
  return segments.length > 0 ? segments : [{ text, match: false }]
}
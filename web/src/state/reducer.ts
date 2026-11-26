// Pure, idempotent projection of server rows into client state. Live deltas,
// snapshot loads, reconnect catch-up and replay frames all go through here, so
// reconnection never duplicates nodes: rows are upserted by id and only
// replaced by rows with an equal or newer sequence.
import type { Delta, Link, Session, Snapshot, Span } from '../api/types'

export interface AtlasState {
  cursor: number
  sessions: Record<string, Session>
  spans: Record<string, Span>
  // links keyed by span id → (resource_id|relation) → link
  links: Record<string, Record<string, Link>>
  // seq that last touched each session (sessions have no seq of their own)
  sessionSeq: Record<string, number>
  eventCount: number
}

export function emptyState(): AtlasState {
  return { cursor: 0, sessions: {}, spans: {}, links: {}, sessionSeq: {}, eventCount: 0 }
}

function upsertSpan(spans: Record<string, Span>, sp: Span): boolean {
  const cur = spans[sp.id]
  if (cur && cur.last_seq > sp.last_seq) return false
  spans[sp.id] = sp
  return true
}

const rank: Record<string, number> = { observed: 3, derived: 2, inferred: 1, unknown: 0 }

function upsertLink(links: AtlasState['links'], l: Link): boolean {
  const m = (links[l.span_id] ??= {})
  const k = `${l.resource_id}|${l.relation}`
  const cur = m[k]
  if (cur && (rank[cur.evidence] ?? 0) > (rank[l.evidence] ?? 0)) return false
  m[k] = l
  return true
}

/** Load a snapshot, replacing state. */
export function applySnapshot(snap: Snapshot): AtlasState {
  const s = emptyState()
  s.cursor = snap.cursor
  for (const ss of snap.sessions) {
    s.sessions[ss.id] = ss
    s.sessionSeq[ss.id] = snap.cursor
  }
  for (const sp of snap.spans) upsertSpan(s.spans, sp)
  for (const l of snap.links) upsertLink(s.links, l)
  return s
}

/** Apply deltas immutably (shallow copies of touched maps). */
export function applyDeltas(prev: AtlasState, deltas: Delta[]): AtlasState {
  if (deltas.length === 0) return prev
  const next: AtlasState = {
    cursor: prev.cursor,
    sessions: { ...prev.sessions },
    spans: { ...prev.spans },
    links: { ...prev.links },
    sessionSeq: { ...prev.sessionSeq },
    eventCount: prev.eventCount,
  }
  for (const d of deltas) {
    if (d.seq <= prev.cursor && prev.cursor > 0 && !d.session && !d.spans) continue
    if (d.session) {
      const known = next.sessionSeq[d.session.id] ?? -1
      if (d.seq >= known) {
        next.sessions[d.session.id] = d.session
        next.sessionSeq[d.session.id] = d.seq
      }
    }
    for (const sp of d.spans ?? []) upsertSpan(next.spans, sp)
    for (const l of d.links ?? []) {
      next.links[l.span_id] = { ...(next.links[l.span_id] ?? {}) }
      upsertLink(next.links, l)
    }
    if (d.seq > next.cursor) {
      next.cursor = d.seq
      next.eventCount++
    }
  }
  return next
}

export function linksOf(state: AtlasState, spanId: string): Link[] {
  return Object.values(state.links[spanId] ?? {})
}

/** Derived session liveness: active (event < idleAfter), idle, stale (open spans but quiet), ended. */
export function sessionLiveness(s: Session, nowUs: number, idleAfterUs = 5 * 60e6): 'active' | 'idle' | 'ended' {
  if (s.status === 'ended') return 'ended'
  return nowUs - s.last_event_at_us > idleAfterUs ? 'idle' : 'active'
}

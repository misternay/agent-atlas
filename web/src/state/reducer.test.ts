import { describe, expect, it } from 'vitest'
import type { Delta, Session, Span } from '../api/types'
import { applyDeltas, applySnapshot, emptyState, linksOf } from './reducer'

const sess = (over: Partial<Session> = {}): Session => ({
  id: 's1', host_id: 'h', harness: 'claude', source_session_id: 'S', workspace: '~/w', status: 'active', start_observed: true,
  started_at_us: 1, last_event_at_us: 1, trace_id: 't', root_span_id: 'r', tool_count: 0, error_count: 0, denied_count: 0, unknown_count: 0, ...over,
})
const span = (over: Partial<Span> = {}): Span => ({
  id: 'sp1', trace_id: 't', session_id: 's1', kind: 'tool', name: 'Bash', status: 'running', status_evidence: 'observed',
  started_at_us: 1, last_seq: 1, ...over,
})
const ev = { schema_version: 1, event_id: 'e', timestamp: '', host_id: 'h', harness: 'claude' as const, type: 'tool.started', evidence: 'observed' as const, privacy: 'metadata_only' }

describe('reducer', () => {
  it('is idempotent: replaying the same deltas does not duplicate or regress', () => {
    const deltas: Delta[] = [
      { seq: 1, event: ev, session: sess(), spans: [span()], links: [{ span_id: 'sp1', resource_id: 'r1', kind: 'file', label: 'a', relation: 'reads', evidence: 'inferred', event_seq: 1 }] },
      { seq: 2, event: ev, session: sess({ tool_count: 1 }), spans: [span({ status: 'completed', last_seq: 2 })], links: [{ span_id: 'sp1', resource_id: 'r1', kind: 'file', label: 'a', relation: 'reads', evidence: 'observed', event_seq: 2 }] },
    ]
    const once = applyDeltas(emptyState(), deltas)
    const twice = applyDeltas(once, deltas)
    expect(Object.keys(twice.spans)).toHaveLength(1)
    expect(twice.spans.sp1.status).toBe('completed')
    expect(linksOf(twice, 'sp1')).toHaveLength(1)
    expect(linksOf(twice, 'sp1')[0].evidence).toBe('observed')
    expect(twice.cursor).toBe(2)
  })

  it('ignores stale rows arriving after newer ones (reconnect catch-up)', () => {
    let s = applyDeltas(emptyState(), [{ seq: 5, event: ev, spans: [span({ status: 'failed', last_seq: 5 })] }])
    s = applyDeltas(s, [{ seq: 3, event: ev, spans: [span({ status: 'running', last_seq: 3 })] }])
    expect(s.spans.sp1.status).toBe('failed')
  })

  it('snapshot then deltas converge', () => {
    let s = applySnapshot({ cursor: 10, sessions: [sess()], spans: [span({ last_seq: 10 })], links: [] })
    s = applyDeltas(s, [{ seq: 11, event: ev, spans: [span({ status: 'unknown', last_seq: 11 })] }])
    expect(s.spans.sp1.status).toBe('unknown')
    expect(s.cursor).toBe(11)
  })
})

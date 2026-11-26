import { describe, expect, it } from 'vitest'
import type { Session, Span } from '../../api/types'
import { applySnapshot } from '../../state/reducer'
import { buildGraph } from './buildGraph'

const NOW = 1_000_000_000_000
const sess = (id: string, over: Partial<Session> = {}): Session => ({
  id, host_id: 'h', harness: 'claude', source_session_id: id, workspace: '~/w', status: 'active', start_observed: true,
  started_at_us: NOW - 1e6, last_event_at_us: NOW - 1e6, trace_id: 't' + id, root_span_id: 'r', tool_count: 0, error_count: 0, denied_count: 0, unknown_count: 0, ...over,
})
const span = (id: string, sid: string, over: Partial<Span> = {}): Span => ({
  id, trace_id: 't', session_id: sid, kind: 'tool', name: 'Read', status: 'completed', status_evidence: 'observed',
  started_at_us: NOW - 2e6, ended_at_us: NOW - 1e6, last_seq: 1, ...over,
})

describe('buildGraph', () => {
  it('renders concurrent sessions with session→tool→resource edges carrying evidence', () => {
    const st = applySnapshot({
      cursor: 3,
      sessions: [sess('a'), sess('b', { harness: 'codex' })],
      spans: [span('x', 'a'), span('y', 'b', { name: 'Bash', status: 'running', ended_at_us: undefined })],
      links: [
        { span_id: 'x', resource_id: 'r1', kind: 'file', label: 'src/a.ts', relation: 'reads', evidence: 'observed', event_seq: 1 },
        { span_id: 'y', resource_id: 'r1', kind: 'file', label: 'src/a.ts', relation: 'reads', evidence: 'inferred', event_seq: 2 },
      ],
    })
    const g = buildGraph(st, { nowUs: NOW, windowS: 60 })
    expect(g.nodes.filter((n) => n.type === 'session')).toHaveLength(2)
    expect(g.nodes.filter((n) => n.type === 'resource')).toHaveLength(1) // shared resource deduped
    const edges = g.edges.filter((e) => e.target === 'res:r1')
    expect(edges.map((e) => e.evidence).sort()).toEqual(['inferred', 'observed'])
  })

  it('keeps running spans outside the live window and folds old finished ones', () => {
    const spans: Span[] = [span('run', 'a', { status: 'running', started_at_us: NOW - 3600e6, ended_at_us: undefined })]
    for (let i = 0; i < 30; i++) spans.push(span('f' + i, 'a', { started_at_us: NOW - 10e6 - i, ended_at_us: NOW - 5e6 }))
    spans.push(span('old', 'a', { started_at_us: NOW - 7200e6, ended_at_us: NOW - 7100e6 }))
    const st = applySnapshot({ cursor: 1, sessions: [sess('a')], spans, links: [] })
    const g = buildGraph(st, { nowUs: NOW, windowS: 60, maxToolsPerSession: 10 })
    const tools = g.nodes.filter((n) => n.type === 'tool')
    expect(tools.some((n) => n.spanId === 'run')).toBe(true)
    expect(tools.some((n) => n.spanId === 'old')).toBe(false)
    expect(tools).toHaveLength(10)
    expect(g.hiddenTools).toBe(21)
  })
})

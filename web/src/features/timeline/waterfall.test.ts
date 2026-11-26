import { describe, expect, it } from 'vitest'
import type { Span } from '../../api/types'
import { waterfallRows } from './Waterfall'

const s = (id: string, start: number, end: number | undefined, parent?: string, kind: Span['kind'] = 'tool'): Span => ({
  id, trace_id: 't', session_id: 's', parent_span_id: parent, kind, name: id, status: end ? 'completed' : 'running',
  status_evidence: 'observed', started_at_us: start, ended_at_us: end, last_seq: 1,
})

describe('waterfallRows', () => {
  it('orders by tree then start time and renders concurrent spans overlapping', () => {
    const { rows } = waterfallRows(
      [s('root', 0, 100, undefined, 'session'), s('turn', 0, 100, 'root', 'turn'), s('b', 20, 60, 'turn'), s('a', 10, 50, 'turn')],
      1000,
    )
    expect(rows.map((r) => r.span.id)).toEqual(['root', 'turn', 'a', 'b'])
    expect(rows.map((r) => r.depth)).toEqual([0, 1, 2, 2])
    const a = rows[2], b = rows[3]
    expect(a.left).toBeCloseTo(0.1)
    expect(b.left).toBeCloseTo(0.2)
    expect(a.left + a.width).toBeGreaterThan(b.left) // overlap visible
  })

  it('extends open spans to now', () => {
    const { rows, t1 } = waterfallRows([s('x', 0, undefined)], 500)
    expect(t1).toBe(500)
    expect(rows[0].open).toBe(true)
    expect(rows[0].width).toBeCloseTo(1)
  })
})

import { useVirtualizer } from '@tanstack/react-virtual'
import { useRef } from 'react'
import type { Span } from '../../api/types'
import { fmtDuration } from '../../format'

export interface WRow {
  span: Span
  depth: number
  left: number // 0..1
  width: number // 0..1
  open: boolean
}

/** Tree-ordered rows with bar geometry. Concurrent spans get overlapping bars on their own rows. */
export function waterfallRows(spans: Span[], nowUs: number): { rows: WRow[]; t0: number; t1: number } {
  if (spans.length === 0) return { rows: [], t0: 0, t1: 0 }
  const t0 = Math.min(...spans.map((s) => s.started_at_us))
  let t1 = Math.max(...spans.map((s) => s.ended_at_us ?? s.started_at_us))
  const anyOpen = spans.some((s) => s.ended_at_us === undefined)
  if (anyOpen) t1 = Math.max(t1, nowUs)
  const span = Math.max(1, t1 - t0)
  const ids = new Set(spans.map((s) => s.id))
  const children = new Map<string, Span[]>()
  const roots: Span[] = []
  for (const s of spans) {
    if (s.parent_span_id && ids.has(s.parent_span_id)) {
      const arr = children.get(s.parent_span_id) ?? []
      arr.push(s)
      children.set(s.parent_span_id, arr)
    } else roots.push(s)
  }
  const byStart = (a: Span, b: Span) => a.started_at_us - b.started_at_us || a.id.localeCompare(b.id)
  const rows: WRow[] = []
  const visit = (s: Span, depth: number) => {
    const end = s.ended_at_us ?? t1
    rows.push({ span: s, depth, left: (s.started_at_us - t0) / span, width: Math.max(0.002, (end - s.started_at_us) / span), open: s.ended_at_us === undefined })
    for (const c of (children.get(s.id) ?? []).sort(byStart)) visit(c, depth + 1)
  }
  for (const r of roots.sort(byStart)) visit(r, 0)
  return { rows, t0, t1 }
}

export function Waterfall({ spans, nowUs, selected, onSelect }: { spans: Span[]; nowUs: number; selected?: string; onSelect: (id: string) => void }) {
  const { rows, t0, t1 } = waterfallRows(spans, nowUs)
  const parent = useRef<HTMLDivElement>(null)
  const v = useVirtualizer({ count: rows.length, getScrollElement: () => parent.current, estimateSize: () => 26, overscan: 12 })
  if (rows.length === 0) return <div className="muted pad">No spans.</div>
  return (
    <div className="waterfall">
      <div className="wf-axis">
        <span>0</span>
        <span>{fmtDuration((t1 - t0) / 2)}</span>
        <span>{fmtDuration(t1 - t0)}</span>
      </div>
      <div className="wf-body" ref={parent}>
        <div style={{ height: v.getTotalSize(), position: 'relative' }}>
          {v.getVirtualItems().map((vi) => {
            const r = rows[vi.index]
            const s = r.span
            return (
              <div
                key={s.id}
                className={`wf-row ${selected === s.id ? 'sel' : ''}`}
                style={{ position: 'absolute', top: 0, left: 0, right: 0, height: 26, transform: `translateY(${vi.start}px)` }}
                onClick={() => onSelect(s.id)}
              >
                <div className="wf-label" style={{ paddingLeft: 6 + r.depth * 14 }} title={s.target_label ?? s.name}>
                  <span className={`kind-${s.kind}`}>{s.kind === 'tool' ? s.name : s.kind}</span>
                  {s.target_label && <span className="muted"> {s.target_label}</span>}
                </div>
                <div className="wf-track">
                  <div className={`wf-bar status-${s.status} ev-${s.status_evidence} ${r.open ? 'open' : ''}`} style={{ left: `${r.left * 100}%`, width: `${r.width * 100}%` }} title={`${s.status} (${s.status_evidence}) · ${fmtDuration(s.duration_us)}`} />
                </div>
                <div className="wf-dur mono">{s.ended_at_us === undefined ? s.status : fmtDuration(s.duration_us)}</div>
              </div>
            )
          })}
        </div>
      </div>
    </div>
  )
}

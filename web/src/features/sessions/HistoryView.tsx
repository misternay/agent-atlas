import { useVirtualizer } from '@tanstack/react-virtual'
import { useEffect, useRef, useState } from 'react'
import { api, type SpanQuery } from '../../api/client'
import type { Session, SpanResult } from '../../api/types'
import { EvidenceBadge, HarnessBadge, StatusBadge } from '../../components/Badges'
import { fmtDateTime, fmtDuration } from '../../format'
import { useStore } from '../../state/store'
import { TraceExplorer } from '../timeline/TraceExplorer'

const RANGES: Record<string, number> = { '1h': 3600, '24h': 86400, '7d': 7 * 86400, '14d': 14 * 86400, all: 0 }

export function HistoryView() {
  const traceId = useStore((s) => s.traceId)
  const openTrace = useStore((s) => s.openTrace)
  const [tab, setTab] = useState<'sessions' | 'calls'>('sessions')
  const [range, setRange] = useState('7d')
  const [harness, setHarness] = useState('')
  const [workspace, setWorkspace] = useState('')
  const [q, setQ] = useState('')
  const [tool, setTool] = useState('')
  const [status, setStatus] = useState('')
  const [resource, setResource] = useState('')
  const [evidence, setEvidence] = useState('')
  const [category, setCategory] = useState('')
  const [sessions, setSessions] = useState<{ list: Session[]; total: number } | null>(null)
  const [calls, setCalls] = useState<{ list: SpanResult[]; total: number } | null>(null)

  const from = RANGES[range] ? (Date.now() - RANGES[range] * 1000) * 1000 : undefined
  useEffect(() => {
    if (traceId) return
    const t = window.setTimeout(() => {
      if (tab === 'sessions') {
        void api.sessions({ harness, workspace, q, from, limit: 500 }).then((r) => setSessions({ list: r.sessions, total: r.total }))
      } else {
        const sq: SpanQuery = { harness, workspace, tool, status, resource, evidence, category, from, limit: 500 }
        void api.search(sq).then((r) => setCalls({ list: r.spans, total: r.total }))
      }
    }, 200)
    return () => window.clearTimeout(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab, range, harness, workspace, q, tool, status, resource, evidence, category, traceId])

  if (traceId) return <TraceExplorer traceId={traceId} onBack={() => openTrace(null)} />

  return (
    <div className="history">
      <div className="filters">
        <div className="seg">
          <button className={tab === 'sessions' ? 'on' : ''} onClick={() => setTab('sessions')}>Sessions</button>
          <button className={tab === 'calls' ? 'on' : ''} onClick={() => setTab('calls')}>Tool calls</button>
        </div>
        <select value={range} onChange={(e) => setRange(e.target.value)} aria-label="time range">
          {Object.keys(RANGES).map((r) => <option key={r} value={r}>{r === 'all' ? 'all time' : `last ${r}`}</option>)}
        </select>
        <select value={harness} onChange={(e) => setHarness(e.target.value)} aria-label="harness">
          <option value="">all harnesses</option>
          <option value="claude">Claude Code</option>
          <option value="codex">Codex</option>
        </select>
        <input placeholder="workspace / project" value={workspace} onChange={(e) => setWorkspace(e.target.value)} />
        {tab === 'sessions' ? (
          <input placeholder="session id" value={q} onChange={(e) => setQ(e.target.value)} />
        ) : (
          <>
            <input placeholder="tool name" value={tool} onChange={(e) => setTool(e.target.value)} />
            <select value={category} onChange={(e) => setCategory(e.target.value)} aria-label="category">
              <option value="">any category</option>
              {['shell', 'file', 'search', 'mcp', 'network', 'agent', 'other'].map((c) => <option key={c}>{c}</option>)}
            </select>
            <select value={status} onChange={(e) => setStatus(e.target.value)} aria-label="status">
              <option value="">any status</option>
              {['running', 'completed', 'failed', 'denied', 'unknown', 'stale'].map((c) => <option key={c}>{c}</option>)}
            </select>
            <input placeholder="resource label / path" value={resource} onChange={(e) => setResource(e.target.value)} />
            <select value={evidence} onChange={(e) => setEvidence(e.target.value)} aria-label="evidence">
              <option value="">any evidence</option>
              {['observed', 'derived', 'inferred', 'unknown'].map((c) => <option key={c}>{c}</option>)}
            </select>
          </>
        )}
      </div>
      {tab === 'sessions' ? <SessionTable data={sessions} onOpen={(s) => openTrace(s.trace_id)} /> : <CallTable data={calls} onOpen={(c) => openTrace(c.trace_id)} />}
    </div>
  )
}

function SessionTable({ data, onOpen }: { data: { list: Session[]; total: number } | null; onOpen: (s: Session) => void }) {
  const parent = useRef<HTMLDivElement>(null)
  const list = data?.list ?? []
  const v = useVirtualizer({ count: list.length, getScrollElement: () => parent.current, estimateSize: () => 40, overscan: 10 })
  if (!data) return <div className="pad muted">Loading…</div>
  return (
    <div className="table">
      <div className="thead srow">
        <span>harness</span><span>workspace</span><span>started</span><span>duration</span><span>tools</span><span>outcomes</span><span>coverage</span>
      </div>
      <div className="tbody" ref={parent}>
        {list.length === 0 && <div className="pad muted">No sessions match.</div>}
        <div style={{ height: v.getTotalSize(), position: 'relative' }}>
          {v.getVirtualItems().map((vi) => {
            const s = list[vi.index]
            return (
              <button key={s.id} className="srow trow" style={{ position: 'absolute', top: 0, left: 0, right: 0, transform: `translateY(${vi.start}px)` }} onClick={() => onOpen(s)}>
                <span><HarnessBadge harness={s.harness} /></span>
                <span className="mono ellip" title={s.workspace}>{s.workspace}</span>
                <span>{fmtDateTime(s.started_at_us)}</span>
                <span>{s.status === 'ended' ? fmtDuration((s.ended_at_us ?? s.last_event_at_us) - s.started_at_us) : <span className="badge status-running">active</span>}</span>
                <span>{s.tool_count}</span>
                <span className="small">
                  {s.error_count > 0 && <span className="badge status-failed">{s.error_count} failed</span>} {s.denied_count > 0 && <span className="badge status-denied">{s.denied_count} denied</span>}{' '}
                  {s.unknown_count > 0 && <span className="badge status-unknown">{s.unknown_count} unknown</span>}
                </span>
                <span className="small muted">{!s.start_observed ? 'start not observed' : s.harness === 'codex' ? 'Codex hook coverage' : 'hooked tools only'}</span>
              </button>
            )
          })}
        </div>
      </div>
      <div className="tfoot muted small">{data.total} sessions{data.total > list.length ? ` (showing ${list.length})` : ''}</div>
    </div>
  )
}

function CallTable({ data, onOpen }: { data: { list: SpanResult[]; total: number } | null; onOpen: (c: SpanResult) => void }) {
  const parent = useRef<HTMLDivElement>(null)
  const list = data?.list ?? []
  const v = useVirtualizer({ count: list.length, getScrollElement: () => parent.current, estimateSize: () => 40, overscan: 10 })
  if (!data) return <div className="pad muted">Loading…</div>
  return (
    <div className="table">
      <div className="thead crow">
        <span>time</span><span>harness</span><span>tool</span><span>target</span><span>status</span><span>evidence</span><span>duration</span>
      </div>
      <div className="tbody" ref={parent}>
        {list.length === 0 && <div className="pad muted">No tool calls match.</div>}
        <div style={{ height: v.getTotalSize(), position: 'relative' }}>
          {v.getVirtualItems().map((vi) => {
            const c = list[vi.index]
            return (
              <button key={c.id} className="crow trow" style={{ position: 'absolute', top: 0, left: 0, right: 0, transform: `translateY(${vi.start}px)` }} onClick={() => onOpen(c)}>
                <span>{fmtDateTime(c.started_at_us)}</span>
                <span><HarnessBadge harness={c.harness} /></span>
                <span className="mono">{c.name}</span>
                <span className="mono ellip" title={c.target_label}>{c.target_label}</span>
                <span><StatusBadge status={c.status} /></span>
                <span><EvidenceBadge evidence={c.status_evidence} /></span>
                <span>{fmtDuration(c.duration_us)}</span>
              </button>
            )
          })}
        </div>
      </div>
      <div className="tfoot muted small">{data.total} calls{data.total > list.length ? ` (showing ${list.length})` : ''}</div>
    </div>
  )
}

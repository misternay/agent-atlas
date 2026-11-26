import { useVirtualizer } from '@tanstack/react-virtual'
import { useMemo, useRef } from 'react'
import { HarnessBadge } from '../../components/Badges'
import { fmtAgo, nowUs } from '../../format'
import { sessionLiveness } from '../../state/reducer'
import { useStore } from '../../state/store'
import type { Session } from '../../api/types'

type Row = { kind: 'header'; label: string; count: number } | { kind: 'session'; s: Session }

export function SessionSidebar() {
  const sessions = useStore((s) => s.live.sessions)
  const spans = useStore((s) => s.live.spans)
  const selected = useStore((s) => s.selectedSession)
  const selectSession = useStore((s) => s.selectSession)
  const select = useStore((s) => s.select)
  const harness = useStore((s) => s.harnessFilter)
  const workspace = useStore((s) => s.workspaceFilter)
  const parent = useRef<HTMLDivElement>(null)
  const now = nowUs()

  const running = useMemo(() => {
    const m: Record<string, number> = {}
    for (const sp of Object.values(spans)) if (sp.kind === 'tool' && (sp.status === 'running' || sp.status === 'stale')) m[sp.session_id] = (m[sp.session_id] ?? 0) + 1
    return m
  }, [spans])

  const rows: Row[] = useMemo(() => {
    const groups = new Map<string, Session[]>()
    for (const s of Object.values(sessions)) {
      if (harness && s.harness !== harness) continue
      if (workspace && !s.workspace.toLowerCase().includes(workspace.toLowerCase())) continue
      const key = `${s.harness === 'claude' ? 'Claude Code' : s.harness === 'codex' ? 'Codex' : s.harness} · ${s.workspace || 'unknown workspace'}`
      const arr = groups.get(key) ?? []
      arr.push(s)
      groups.set(key, arr)
    }
    const out: Row[] = []
    const keys = [...groups.keys()].sort((a, b) => Math.max(...groups.get(b)!.map((s) => s.last_event_at_us)) - Math.max(...groups.get(a)!.map((s) => s.last_event_at_us)))
    for (const k of keys) {
      const list = groups.get(k)!.sort((a, b) => b.last_event_at_us - a.last_event_at_us)
      out.push({ kind: 'header', label: k, count: list.length })
      for (const s of list) out.push({ kind: 'session', s })
    }
    return out
  }, [sessions, harness, workspace])

  const v = useVirtualizer({ count: rows.length, getScrollElement: () => parent.current, estimateSize: (i) => (rows[i].kind === 'header' ? 30 : 58), overscan: 8 })

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <span>Sessions</span>
        {selected && (
          <button className="link" onClick={() => selectSession(null)}>
            show all
          </button>
        )}
      </div>
      <div className="sidebar-list" ref={parent}>
        {rows.length === 0 && <div className="muted pad">No sessions yet.</div>}
        <div style={{ height: v.getTotalSize(), position: 'relative' }}>
          {v.getVirtualItems().map((vi) => {
            const r = rows[vi.index]
            const style: React.CSSProperties = { position: 'absolute', top: 0, left: 0, right: 0, transform: `translateY(${vi.start}px)`, height: vi.size }
            if (r.kind === 'header')
              return (
                <div key={vi.key} style={style} className="group-head" title={r.label}>
                  {r.label} <span className="muted">({r.count})</span>
                </div>
              )
            const s = r.s
            const live = sessionLiveness(s, now)
            return (
              <button
                key={vi.key}
                style={style}
                className={`session-row ${selected === s.id ? 'sel' : ''} live-${live}`}
                onClick={() => {
                  selectSession(selected === s.id ? null : s.id)
                  select({ kind: 'session', id: s.id })
                }}
              >
                <div className="sr-top">
                  <span className={`dot live-${live}`} title={live} />
                  <HarnessBadge harness={s.harness} />
                  <span className="muted mono">{s.source_session_id.slice(0, 8)}</span>
                  {running[s.id] ? <span className="badge status-running">{running[s.id]} running</span> : null}
                </div>
                <div className="sr-bottom muted">
                  {live} · {s.tool_count} tools{s.error_count ? ` · ${s.error_count} failed` : ''}
                  {s.unknown_count ? ` · ${s.unknown_count} unknown` : ''} · {fmtAgo(s.last_event_at_us, now)}
                </div>
              </button>
            )
          })}
        </div>
      </div>
    </aside>
  )
}

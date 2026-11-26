import { useEffect, useState } from 'react'
import { api } from '../../api/client'
import type { Link, Session, Span, StoredEvent } from '../../api/types'
import { EvidenceBadge, HarnessBadge, StatusBadge } from '../../components/Badges'
import { fmtDateTime, fmtDuration, fmtTime } from '../../format'
import { linksOf, type AtlasState } from '../../state/reducer'
import { useStore, type Selection } from '../../state/store'

interface Props {
  selection: Selection | null
  state?: AtlasState // defaults to live state
  spanLinks?: Record<string, Link[]>
  extraSpans?: Record<string, Span>
  extraSessions?: Record<string, Session>
  onClose?: () => void
}

export function Inspector({ selection, state, spanLinks, extraSpans, extraSessions, onClose }: Props) {
  const live = useStore((s) => s.live)
  const openTrace = useStore((s) => s.openTrace)
  const st = state ?? live
  if (!selection) return <aside className="inspector empty-insp muted">Select a session, tool call or resource to inspect its source events and evidence.</aside>
  const span = selection.kind === 'span' ? (extraSpans?.[selection.id] ?? st.spans[selection.id]) : undefined
  const sessionId = selection.kind === 'session' ? selection.id : span?.session_id
  const session = sessionId ? (extraSessions?.[sessionId] ?? st.sessions[sessionId]) : undefined
  return (
    <aside className="inspector">
      <div className="insp-head">
        <span>Inspector</span>
        {onClose && (
          <button className="link" onClick={onClose}>
            close
          </button>
        )}
      </div>
      {selection.kind === 'span' && span && <SpanDetails span={span} session={session} links={spanLinks?.[span.id] ?? linksOf(st, span.id)} onOpenTrace={() => openTrace(span.trace_id)} />}
      {selection.kind === 'span' && !span && <div className="muted pad">Span no longer in view.</div>}
      {selection.kind === 'session' && session && <SessionDetails s={session} onOpenTrace={() => openTrace(session.trace_id)} />}
      {selection.kind === 'resource' && <ResourceDetails id={selection.id} state={st} />}
    </aside>
  )
}

function Row({ k, children }: { k: string; children: React.ReactNode }) {
  return (
    <div className="kv">
      <div className="k">{k}</div>
      <div className="v">{children}</div>
    </div>
  )
}

function SpanDetails({ span, session, links, onOpenTrace }: { span: Span; session?: Session; links: Link[]; onOpenTrace: () => void }) {
  const [events, setEvents] = useState<StoredEvent[] | null>(null)
  useEffect(() => {
    let cancelled = false
    setEvents(null)
    if (!span.session_id) return
    // Source events: everything in this session that touched this span.
    void (async () => {
      const out: StoredEvent[] = []
      let after = Math.max(0, (span.start_seq || span.last_seq) - 1)
      for (let i = 0; i < 5; i++) {
        const r = await api.sessionEvents(span.session_id, after, 500).catch(() => null)
        if (!r || r.events.length === 0) break
        for (const e of r.events) if (e.span_id === span.id) out.push(e)
        after = r.next_after_seq
        if (after >= span.last_seq) break
      }
      if (!cancelled) setEvents(out)
    })()
    return () => {
      cancelled = true
    }
  }, [span.id, span.session_id, span.start_seq, span.last_seq])
  return (
    <div className="insp-body">
      <h3 className="insp-title">{span.name}</h3>
      {span.target_label && <div className="mono target">{span.target_label}</div>}
      <Row k="status">
        <StatusBadge status={span.status} /> <EvidenceBadge evidence={span.status_evidence} />
      </Row>
      {span.outcome_basis && <Row k="outcome basis">{span.outcome_basis.replaceAll('_', ' ')}</Row>}
      <Row k="category">{[span.tool_category, span.operation].filter(Boolean).join(' · ') || '—'}</Row>
      <Row k="started">{fmtTime(span.started_at_us)}{span.attrs?.start_missing === 'true' && <span className="warn"> (start not observed)</span>}</Row>
      <Row k="ended">{fmtTime(span.ended_at_us)}</Row>
      <Row k="duration">{fmtDuration(span.duration_us)}</Row>
      {session && (
        <Row k="session">
          <HarnessBadge harness={session.harness} /> <span className="mono">{session.source_session_id.slice(0, 12)}</span>
        </Row>
      )}
      <Row k="tool call id"><span className="mono">{span.tool_call_id || '—'}</span>{span.attrs?.uncorrelated === 'true' && <span className="warn"> (no call id: not correlatable)</span>}</Row>
      <Row k="trace id">
        <button className="link mono" onClick={onOpenTrace}>{span.trace_id.slice(0, 16)}…</button>
      </Row>
      {span.attrs?.mcp_server && <Row k="MCP">{span.attrs.mcp_server} › {span.attrs.mcp_tool}</Row>}
      <h4>Resources</h4>
      {links.length === 0 ? (
        <div className="muted small">No resource links observed for this call.</div>
      ) : (
        <ul className="res-list">
          {links.map((l) => (
            <li key={l.resource_id + l.relation}>
              <span className="rel">{l.relation}</span> <span className="mono">{l.label}</span> <EvidenceBadge evidence={l.evidence} />
              <span className="muted small"> ({l.kind}, from event #{l.event_seq})</span>
            </li>
          ))}
        </ul>
      )}
      {links.some((l) => l.evidence === 'inferred') && (
        <p className="caveat small">Inferred links come from parsing a shell command; the process may have touched other files that hooks cannot see.</p>
      )}
      <h4>Source events</h4>
      {events === null ? (
        <div className="muted small">loading…</div>
      ) : events.length === 0 ? (
        <div className="muted small">No stored events (pruned or not yet committed).</div>
      ) : (
        <table className="ev-table">
          <thead>
            <tr><th>#</th><th>type</th><th>hook</th><th>evidence</th><th>time</th></tr>
          </thead>
          <tbody>
            {events.map((e) => (
              <tr key={e.seq}>
                <td className="mono">{e.seq}</td>
                <td>{e.event.type}</td>
                <td className="mono small">{e.event.source_event}</td>
                <td><EvidenceBadge evidence={e.event.evidence} /></td>
                <td className="mono small">{fmtTime(Date.parse(e.event.timestamp) * 1000)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

function SessionDetails({ s, onOpenTrace }: { s: Session; onOpenTrace: () => void }) {
  return (
    <div className="insp-body">
      <h3 className="insp-title"><HarnessBadge harness={s.harness} /> session</h3>
      <Row k="workspace"><span className="mono">{s.workspace || '—'}</span></Row>
      <Row k="status">{s.status}</Row>
      <Row k="session id"><span className="mono">{s.source_session_id}</span></Row>
      <Row k="started">{fmtDateTime(s.started_at_us)}{!s.start_observed && <span className="warn"> (start not observed)</span>}</Row>
      <Row k="ended">{fmtDateTime(s.ended_at_us)}</Row>
      <Row k="last event">{fmtDateTime(s.last_event_at_us)}</Row>
      <Row k="tools">{s.tool_count} calls · {s.error_count} failed · {s.denied_count} denied · {s.unknown_count} unknown</Row>
      {s.model && <Row k="model">{s.model}</Row>}
      {s.attrs?.source && <Row k="start source">{s.attrs.source}</Row>}
      {s.attrs?.end_reason && <Row k="end reason">{s.attrs.end_reason}</Row>}
      <Row k="trace">
        <button className="link mono" onClick={onOpenTrace}>open trace timeline →</button>
      </Row>
      <p className="caveat small">Coverage: only hook-instrumented tool surfaces are visible. Shell commands may access files or networks internally without hook events.</p>
    </div>
  )
}

function ResourceDetails({ id, state }: { id: string; state: AtlasState }) {
  const uses: { span: Span; link: Link }[] = []
  for (const [spanId, m] of Object.entries(state.links)) {
    for (const l of Object.values(m)) if (l.resource_id === id && state.spans[spanId]) uses.push({ span: state.spans[spanId], link: l })
  }
  const first = uses[0]?.link
  const select = useStore((s) => s.select)
  return (
    <div className="insp-body">
      <h3 className="insp-title mono">{first?.label ?? id}</h3>
      <Row k="kind">{first?.kind ?? '—'}</Row>
      <h4>Linked calls</h4>
      <ul className="res-list">
        {uses.map(({ span, link }) => (
          <li key={span.id + link.relation}>
            <button className="link" onClick={() => select({ kind: 'span', id: span.id })}>{span.name}</button> <span className="rel">{link.relation}</span> <EvidenceBadge evidence={link.evidence} />{' '}
            <StatusBadge status={span.status} />
          </li>
        ))}
      </ul>
    </div>
  )
}

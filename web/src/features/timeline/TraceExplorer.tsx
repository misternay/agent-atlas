import { useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../../api/client'
import type { Delta, Link, Session, Span } from '../../api/types'
import { EvidenceBadge, HarnessBadge, StatusBadge } from '../../components/Badges'
import { fmtDateTime, fmtDuration, nowUs } from '../../format'
import { applyDeltas, emptyState, type AtlasState } from '../../state/reducer'
import { useStore, type Selection } from '../../state/store'
import { Inspector } from '../inspector/Inspector'
import { Waterfall } from './Waterfall'

/** Trace explorer: waterfall timeline, span list, inspector, deterministic replay. */
export function TraceExplorer({ traceId, onBack }: { traceId: string; onBack: () => void }) {
  const [data, setData] = useState<{ session: Session; spans: Span[]; links: Link[] } | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [selection, setSelection] = useState<Selection | null>(null)
  const [mode, setMode] = useState<'trace' | 'replay'>('trace')
  const [exportError, setExportError] = useState(false)
  const liveCursor = useStore((s) => s.live.cursor)

  useEffect(() => {
    let cancel = false
    api
      .trace(traceId)
      .then((d) => !cancel && setData(d))
      .catch((e) => !cancel && setErr(String(e)))
    return () => {
      cancel = true
    }
    // refresh while the session is live
  }, [traceId, mode === 'trace' ? Math.floor(liveCursor / 5) : 0])

  const linkMap = useMemo(() => {
    const m: Record<string, Link[]> = {}
    for (const l of data?.links ?? []) (m[l.span_id] ??= []).push(l)
    return m
  }, [data])
  const spanMap = useMemo(() => Object.fromEntries((data?.spans ?? []).map((s) => [s.id, s])), [data])

  if (err) return <div className="pad error">Could not load trace: {err}</div>
  if (!data) return <div className="pad muted">Loading trace…</div>
  const s = data.session
  async function downloadOTLP() {
    setExportError(false)
    try {
      const json = await api.traceOTLP(traceId)
      const url = URL.createObjectURL(new Blob([JSON.stringify(json, null, 2)], { type: 'application/json' }))
      const link = document.createElement('a')
      link.href = url
      link.download = `atlas-${traceId}.json`
      link.click()
      window.setTimeout(() => URL.revokeObjectURL(url), 1000)
    } catch {
      setExportError(true)
    }
  }
  return (
    <div className="trace">
      <div className="trace-head">
        <button className="link" onClick={onBack}>← sessions</button>
        <HarnessBadge harness={s.harness} />
        <span className="mono">{s.workspace}</span>
        <span className="muted">{fmtDateTime(s.started_at_us)} · {s.status === 'ended' ? fmtDuration((s.ended_at_us ?? s.last_event_at_us) - s.started_at_us) : 'active'}</span>
        <span className="muted">{s.tool_count} tools · {s.error_count} failed · {s.denied_count} denied · {s.unknown_count} unknown</span>
        <span className="spacer" />
        <div className="seg">
          <button className={mode === 'trace' ? 'on' : ''} onClick={() => setMode('trace')}>Timeline</button>
          <button className={mode === 'replay' ? 'on' : ''} onClick={() => setMode('replay')}>Replay</button>
        </div>
        {exportError && <span role="alert" className="small error">Could not download OTLP JSON.</span>}
        <button className="link small" onClick={() => void downloadOTLP()}>Download OTLP JSON</button>
      </div>
      {mode === 'trace' ? (
        <div className="trace-main">
          <div className="trace-left">
            <Waterfall spans={data.spans} nowUs={nowUs()} selected={selection?.kind === 'span' ? selection.id : undefined} onSelect={(id) => setSelection({ kind: 'span', id })} />
          </div>
          <Inspector selection={selection} spanLinks={linkMap} extraSpans={spanMap} extraSessions={{ [s.id]: s }} onClose={() => setSelection(null)} state={emptyState()} />
        </div>
      ) : (
        <Replay sessionId={s.id} />
      )}
    </div>
  )
}

function Replay({ sessionId }: { sessionId: string }) {
  const [frames, setFrames] = useState<Delta[] | null>(null)
  const [truncated, setTruncated] = useState(false)
  const [error, setError] = useState(false)
  const [i, setI] = useState(0)
  const [playing, setPlaying] = useState(false)
  const [speed, setSpeed] = useState(4)
  const [selection, setSelection] = useState<Selection | null>(null)
  const timer = useRef<number | null>(null)

  useEffect(() => {
    let cancel = false
    api.replay(sessionId).then((r) => {
      if (cancel) return
      setFrames(r.frames)
      setTruncated(r.truncated)
      setI(r.frames.length)
    }).catch(() => { if (!cancel) setError(true) })
    return () => {
      cancel = true
    }
  }, [sessionId])

  // Deterministic: state at step i is the fold of frames[0..i) through the shared reducer.
  const state: AtlasState = useMemo(() => (frames ? applyDeltas(emptyState(), frames.slice(0, i)) : emptyState()), [frames, i])
  const current = frames && i > 0 ? frames[i - 1] : null

  useEffect(() => {
    if (!playing || !frames) return
    timer.current = window.setInterval(() => {
      setI((x) => {
        if (x >= frames.length) {
          setPlaying(false)
          return x
        }
        return x + 1
      })
    }, 1000 / speed)
    return () => {
      if (timer.current) window.clearInterval(timer.current)
    }
  }, [playing, speed, frames])

  if (error) return <div role="alert" className="pad error">Could not load replay. Reopen the trace to try again.</div>
  if (!frames) return <div className="pad muted">Loading stored events…</div>
  const spans = Object.values(state.spans)
  const atUs = current ? Date.parse(current.event.timestamp) * 1000 : 0
  return (
    <div className="replay">
      {truncated && <div className="replay-banner">Showing the first {frames.length.toLocaleString()} stored events. This session contains more events.</div>}
      <div className="replay-controls">
        <button onClick={() => { setI(0); setPlaying(false) }}>⏮</button>
        <button onClick={() => setI((x) => Math.max(0, x - 1))}>◀</button>
        <button onClick={() => { if (i >= frames.length) setI(0); setPlaying((p) => !p) }}>{playing ? '❚❚' : '▶'}</button>
        <button onClick={() => setI((x) => Math.min(frames.length, x + 1))}>▶|</button>
        <input type="range" min={0} max={frames.length} value={i} onChange={(e) => { setPlaying(false); setI(Number(e.target.value)) }} />
        <span className="mono small">{i}/{frames.length}</span>
        <select value={speed} onChange={(e) => setSpeed(Number(e.target.value))}>
          {[1, 2, 4, 10, 30].map((x) => <option key={x} value={x}>{x} ev/s</option>)}
        </select>
      </div>
      {current && (
        <div className="replay-current">
          <span className="mono">#{current.seq}</span> <b>{current.event.type}</b> <span className="muted">{current.event.source_event}</span>{' '}
          {current.event.tool && <span>{current.event.tool.name}</span>} {current.event.target && <span className="mono muted">{current.event.target.label}</span>}{' '}
          {current.event.status && <StatusBadge status={current.event.status} />} <EvidenceBadge evidence={current.event.evidence} />
        </div>
      )}
      <div className="trace-main">
        <div className="trace-left">
          <Waterfall spans={spans} nowUs={atUs || nowUs()} selected={selection?.kind === 'span' ? selection.id : undefined} onSelect={(id) => setSelection({ kind: 'span', id })} />
        </div>
        <Inspector selection={selection} state={state} onClose={() => setSelection(null)} />
      </div>
    </div>
  )
}

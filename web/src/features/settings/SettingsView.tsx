import { useEffect, useState } from 'react'
import { api } from '../../api/client'
import type { Capability, Health, Settings } from '../../api/types'
import { fmtBytes, fmtDateTime } from '../../format'
import { useStore } from '../../state/store'

export function SettingsView() {
  const [settings, setSettings] = useState<Settings | null>(null)
  const [health, setHealth] = useState<Health | null>(null)
  const [caps, setCaps] = useState<Capability[]>([])
  const [confirm, setConfirm] = useState('')
  const [msg, setMsg] = useState('')
  const paused = useStore((s) => s.collectionPaused)
  const setPaused = useStore((s) => s.setCollectionPaused)
  const resync = useStore((s) => s.resync)

  const load = () => {
    void api.settings().then(setSettings)
    void api.health().then(setHealth)
    void api.capabilities().then((r) => setCaps(r.harnesses))
  }
  useEffect(load, [])

  const storage = (health?.storage ?? {}) as Record<string, number>
  return (
    <div className="settings">
      <section>
        <h3>Collection</h3>
        <p>
          Collection is <b>{paused ? 'paused' : 'active'}</b>. While paused, hooks drop events immediately and nothing is stored.
        </p>
        <button onClick={() => void setPaused(!paused)}>{paused ? 'Resume collection' : 'Pause collection'}</button>
      </section>
      <section>
        <h3>Storage</h3>
        <div className="grid2">
          <span>Database</span><span>{fmtBytes((storage.db_bytes ?? 0) + (storage.wal_bytes ?? 0))} of {fmtBytes(settings?.max_db_bytes)} target</span>
          <span>Sessions / events</span><span>{storage.sessions ?? 0} / {storage.events ?? 0}</span>
          <span>Oldest event</span><span>{fmtDateTime(storage.oldest_event_us)}</span>
          <span>Retention</span><span>{settings?.retention_days} days</span>
          <span>Offline spool</span><span>{fmtBytes(health?.spool_bytes)} (cap {fmtBytes(settings?.spool_max_bytes)})</span>
          <span>Stale after</span><span>{settings?.stale_after_secs}s without completion</span>
          <span>Data directory</span><span className="mono small">{settings?.home}</span>
        </div>
        {settings?.retention && (
          <p className="small muted">
            Last retention run {String(settings.retention.ran_at ?? '')}: removed {String(settings.retention.removed_sessions ?? 0)} sessions and {String(settings.retention.removed_events ?? 0)} events.
          </p>
        )}
      </section>
      <section>
        <h3>What Atlas stores</h3>
        <div className="grid2 cols">
          <div>
            <h4>Stored (sanitized metadata)</h4>
            <ul>{settings?.stored_fields.map((f) => <li key={f}>{f}</li>)}</ul>
          </div>
          <div>
            <h4>Never stored</h4>
            <ul>{settings?.never_stored.map((f) => <li key={f}>{f}</li>)}</ul>
          </div>
        </div>
        <p className="small muted">
          Raw prompts: {settings?.raw_prompts ? 'on' : 'off'} · Raw tool output: {settings?.raw_tool_output ? 'on' : 'off'} · External export: {settings?.export_enabled ? 'on' : 'off'}
        </p>
      </section>
      <section>
        <h3>Harness coverage</h3>
        {caps.map((c) => (
          <div key={c.harness} className="cap">
            <h4>{c.harness === 'claude' ? 'Claude Code' : 'Codex'}</h4>
            <div className="small">
              tool start {yn(c.capabilities.tool_start)} · finish {yn(c.capabilities.tool_finish)} · explicit failure {yn(c.capabilities.tool_failure)} · denial {yn(c.capabilities.tool_denied)} · MCP {yn(c.capabilities.mcp_calls)} · subagents {yn(c.capabilities.subagents)}
            </div>
            <div className="small muted">Observed hooks: {Object.keys(c.observed_hooks ?? {}).join(', ') || 'none yet'}</div>
            {c.not_yet_observed && c.not_yet_observed.length > 0 && <div className="small muted">Supported hooks not yet seen: {c.not_yet_observed.join(', ')}</div>}
            <ul className="small">{c.capabilities.notes?.map((n) => <li key={n}>{n}</li>)}</ul>
          </div>
        ))}
      </section>
      <section className="danger">
        <h3>Clear history</h3>
        <p>Permanently deletes all stored sessions, events and spans. This cannot be undone.</p>
        <input placeholder='type "delete all history"' value={confirm} onChange={(e) => setConfirm(e.target.value)} />
        <button
          className="danger-btn"
          disabled={confirm !== 'delete all history'}
          onClick={() =>
            void api.clearHistory().then(() => {
              setMsg('History cleared.')
              setConfirm('')
              load()
              void resync()
            })
          }
        >
          Delete all history
        </button>
        {msg && <span className="small"> {msg}</span>}
      </section>
    </div>
  )
}

const yn = (b: boolean) => (b ? '✓' : '—')

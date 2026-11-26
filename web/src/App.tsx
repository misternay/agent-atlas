import { useEffect } from 'react'
import { LiveGraph } from './features/live-graph/LiveGraph'
import { Inspector } from './features/inspector/Inspector'
import { HistoryView } from './features/sessions/HistoryView'
import { SessionSidebar } from './features/sessions/SessionSidebar'
import { SettingsView } from './features/settings/SettingsView'
import { useStore } from './state/store'

export function App() {
  const view = useStore((s) => s.view)
  const setView = useStore((s) => s.setView)
  const boot = useStore((s) => s.boot)
  const authError = useStore((s) => s.authError)
  const connected = useStore((s) => s.connected)
  const collectionPaused = useStore((s) => s.collectionPaused)
  const livePaused = useStore((s) => s.livePaused)
  const pending = useStore((s) => s.pending.length)
  const toggleLivePause = useStore((s) => s.toggleLivePause)
  const harness = useStore((s) => s.harnessFilter)
  const workspace = useStore((s) => s.workspaceFilter)
  const search = useStore((s) => s.search)
  const windowS = useStore((s) => s.windowS)
  const setFilter = useStore((s) => s.setFilter)
  const selection = useStore((s) => s.selection)
  const select = useStore((s) => s.select)
  const openTrace = useStore((s) => s.openTrace)

  useEffect(() => {
    void boot()
  }, [boot])

  if (authError)
    return (
      <div className="auth-error">
        <h2>Agent Atlas</h2>
        <p>Sign in to view local telemetry.</p>
        <p>
          Run <code>atlas open</code> in a terminal to sign in.
        </p>
      </div>
    )

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">Agent Atlas</div>
        <nav className="tabs">
          <button className={view === 'live' ? 'on' : ''} onClick={() => setView('live')}>Live</button>
          <button className={view === 'history' ? 'on' : ''} onClick={() => { openTrace(null); setView('history') }}>History</button>
          <button className={view === 'settings' ? 'on' : ''} onClick={() => setView('settings')}>Settings</button>
        </nav>
        {view === 'live' && (
          <div className="controls">
            <select value={harness} onChange={(e) => setFilter({ harnessFilter: e.target.value })} aria-label="harness filter">
              <option value="">all harnesses</option>
              <option value="claude">Claude Code</option>
              <option value="codex">Codex</option>
            </select>
            <input placeholder="workspace" value={workspace} onChange={(e) => setFilter({ workspaceFilter: e.target.value })} />
            <input placeholder="search tools / files" value={search} onChange={(e) => setFilter({ search: e.target.value })} />
            <select value={windowS} onChange={(e) => setFilter({ windowS: Number(e.target.value) })} aria-label="live window">
              {[30, 60, 300, 900, 3600].map((w) => <option key={w} value={w}>{w < 60 ? `${w}s` : `${w / 60}m`} window</option>)}
            </select>
            <button onClick={toggleLivePause} className={livePaused ? 'warn-btn' : ''} title="Pause rendering of live updates (collection continues)">
              {livePaused ? `Resume view (${pending})` : 'Pause view'}
            </button>
          </div>
        )}
        <span className="spacer" />
        <span className={`conn ${connected ? 'ok' : 'down'}`} title={connected ? 'live stream connected' : 'reconnecting…'}>
          {connected ? 'live' : 'reconnecting'}
        </span>
        {collectionPaused && <span className="badge status-stale" title="Hooks are dropping events">collection paused</span>}
      </header>
      {view === 'live' && (
        <main className="live">
          <SessionSidebar />
          <LiveGraph />
          <Inspector selection={selection} onClose={() => select(null)} />
        </main>
      )}
      {view === 'history' && <HistoryView />}
      {view === 'settings' && <SettingsView />}
    </div>
  )
}

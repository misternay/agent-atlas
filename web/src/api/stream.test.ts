import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AtlasStream } from './stream'

// Minimal fake WebSocket so the reconnect/resync state machine can be tested.
class FakeWS {
  static all: FakeWS[] = []
  url: string
  onopen: (() => void) | null = null
  onmessage: ((e: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  closed = false
  constructor(url: string) {
    this.url = url
    FakeWS.all.push(this)
  }
  close() {
    if (this.closed) return
    this.closed = true
    this.onclose?.()
  }
  send(m: object) {
    this.onmessage?.({ data: JSON.stringify(m) })
  }
}

const open = () => FakeWS.all.filter((w) => !w.closed)

describe('AtlasStream', () => {
  beforeEach(() => {
    FakeWS.all = []
    vi.useFakeTimers()
    vi.stubGlobal('WebSocket', FakeWS)
    vi.stubGlobal('window', globalThis)
    vi.stubGlobal('location', { protocol: 'http:', host: '127.0.0.1:7878' })
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('restart() cancels a pending reconnect timer (never two sockets)', () => {
    const s = new AtlasStream({ onDeltas: () => {}, onResync: () => {}, onStatus: () => {} })
    s.start(5, 0)
    FakeWS.all[0].close() // drop → reconnect scheduled
    s.restart(10, 0) // snapshot reload happens before the timer fires
    vi.advanceTimersByTime(20_000)
    expect(open()).toHaveLength(1)
    expect(open()[0].url).toContain('after_seq=10')
  })

  it('does not reconnect with stale state after resync_required; resumes from the new cursor', () => {
    const onResync = vi.fn()
    const s = new AtlasStream({ onDeltas: () => {}, onResync, onStatus: () => {} })
    s.start(5, 0)
    const ws = FakeWS.all[0]
    ws.send({ type: 'resync_required', reason: 'history_changed' })
    ws.close() // server closes after asking for a resync
    vi.advanceTimersByTime(20_000)
    expect(onResync).toHaveBeenCalledTimes(1)
    expect(open()).toHaveLength(0) // waits for the app's restart()
    s.restart(42, 3)
    expect(open()[0].url).toContain('after_seq=42&gen=3')
  })

  it('asks the app to re-check auth after repeated connection failures', () => {
    const onResync = vi.fn()
    const s = new AtlasStream({ onDeltas: () => {}, onResync, onStatus: () => {} })
    s.start(0, 0)
    for (let i = 0; i < 3; i++) {
      open()[0]?.close()
      vi.advanceTimersByTime(20_000)
    }
    expect(onResync).toHaveBeenCalledWith('reconnect_failed')
  })

  it('backs off after a successful snapshot when WebSocket is still unavailable', () => {
    const onResync = vi.fn(() => s.restart(30, 2))
    const s = new AtlasStream({ onDeltas: () => {}, onResync, onStatus: () => {} })
    s.start(0, 0)
    open()[0].close()
    vi.advanceTimersByTime(250)
    open()[0].close()
    vi.advanceTimersByTime(500)
    open()[0].close()
    expect(onResync).toHaveBeenCalledWith('reconnect_failed')
    expect(open()).toHaveLength(0)
    vi.advanceTimersByTime(2_000)
    expect(open()[0].url).toContain('after_seq=30&gen=2')
    open()[0].close()
    vi.advanceTimersByTime(1_000)
    expect(open()).toHaveLength(0)
    expect(onResync).toHaveBeenCalledTimes(1)
  })

  it('batches deltas and advances the cursor', () => {
    const onDeltas = vi.fn()
    const s = new AtlasStream({ onDeltas, onResync: () => {}, onStatus: () => {} }, 100)
    s.start(0, 0)
    const ev = { schema_version: 1, event_id: 'e', timestamp: '', host_id: 'h', harness: 'claude', type: 'tool.started', evidence: 'observed', privacy: 'metadata_only' }
    FakeWS.all[0].onopen?.()
    FakeWS.all[0].send({ type: 'delta', seq: 1, event: ev })
    FakeWS.all[0].send({ type: 'delta', seq: 2, event: ev })
    expect(onDeltas).not.toHaveBeenCalled()
    vi.advanceTimersByTime(150)
    expect(onDeltas).toHaveBeenCalledTimes(1)
    expect(onDeltas.mock.calls[0][0]).toHaveLength(2)
    FakeWS.all[0].close()
    vi.advanceTimersByTime(300)
    expect(open()[0].url).toContain('after_seq=2')
  })
})

// Resumable WebSocket client: reconnects with the last committed cursor and
// asks the app to reload a snapshot when the server reports resync_required.
import type { Delta, StreamMessage } from './types'
import { sessionSecret } from './session'

export interface StreamHandlers {
  onDeltas: (d: Delta[]) => void
  onResync: (reason: string) => void
  onStatus: (s: { connected: boolean; paused?: boolean; lagging?: boolean }) => void
}

export class AtlasStream {
  private ws: WebSocket | null = null
  private cursor = 0
  private gen = 0
  private stopped = false
  private retry = 0
  private buf: Delta[] = []
  private flushTimer: number | null = null
  private reconnectTimer: number | null = null
  // Set when the server asked for a resync: the app reloads a snapshot and
  // calls restart(); until then we must not reconnect with stale state.
  private awaitingResync = false
  private h: StreamHandlers
  private batchMs: number

  constructor(h: StreamHandlers, batchMs = 150) {
    this.h = h
    this.batchMs = batchMs
  }

  start(cursor: number, gen: number) {
    this.cursor = cursor
    this.gen = gen
    this.stopped = false
    this.awaitingResync = false
    this.connect()
  }

  /** Reconnect from a fresh snapshot's cursor so nothing committed during the
   * snapshot fetch is lost (the server catches up from the cursor). */
  restart(cursor: number, gen: number) {
    this.stop()
    this.cursor = cursor
    this.gen = gen
    this.stopped = false
    this.awaitingResync = false
    if (this.retry > 0) this.scheduleReconnect()
    else this.connect()
  }

  stop() {
    this.stopped = true
    if (this.flushTimer !== null) window.clearTimeout(this.flushTimer)
    this.flushTimer = null
    this.buf = []
    if (this.reconnectTimer !== null) {
      window.clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    const ws = this.ws
    this.ws = null
    ws?.close()
  }

  setCursor(c: number) {
    this.cursor = c
  }

  private connect() {
    this.reconnectTimer = null
    if (this.stopped || this.ws) return
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const session = sessionSecret()
    const protocols = session ? ['atlas', `atlas-session.${session}`] : undefined
    const ws = new WebSocket(`${proto}://${location.host}/api/v1/stream?after_seq=${this.cursor}&gen=${this.gen}`, protocols)
    this.ws = ws
    ws.onopen = () => {
      if (this.ws !== ws) return
      this.h.onStatus({ connected: true })
    }
    ws.onmessage = (e) => {
      if (this.ws !== ws) return // superseded by restart()
      let m: StreamMessage
      try {
        m = JSON.parse(e.data as string)
      } catch {
        return
      }
      switch (m.type) {
        case 'hello':
          this.retry = 0
          this.h.onStatus({ connected: true, paused: m.paused })
          break
        case 'status':
          this.h.onStatus({ connected: true, paused: m.paused })
          break
        case 'resync_required':
          this.flush()
          this.awaitingResync = true
          this.h.onResync(m.reason ?? 'resync')
          break
        case 'delta':
          if (m.seq && m.event) {
            this.buf.push({ seq: m.seq, event: m.event, session: m.session, spans: m.spans, links: m.links })
            if (m.seq > this.cursor) this.cursor = m.seq
            this.schedule()
          }
          break
      }
    }
    ws.onclose = () => {
      if (this.ws !== ws) return // superseded by restart()
      this.flush()
      this.ws = null
      this.h.onStatus({ connected: false })
      if (this.stopped || this.awaitingResync) return
      // Repeated failures may mean the session expired (e.g. daemon restart):
      // let the app re-check auth via a snapshot reload.
      if (this.retry === 2 || this.retry === 6) {
        this.retry = Math.min(this.retry + 1, 6)
        this.awaitingResync = true
        this.h.onResync('reconnect_failed')
        return
      }
      this.scheduleReconnect()
    }
    ws.onerror = () => ws.close()
  }

  private scheduleReconnect() {
    const delay = Math.min(10_000, 250 * 2 ** this.retry)
    this.retry = Math.min(this.retry + 1, 6)
    this.reconnectTimer = window.setTimeout(() => this.connect(), delay)
  }

  // Batch UI updates (~100–250 ms target, design §8.3).
  private schedule() {
    if (this.flushTimer !== null) return
    this.flushTimer = window.setTimeout(() => this.flush(), this.batchMs)
  }

  private flush() {
    if (this.flushTimer !== null) {
      window.clearTimeout(this.flushTimer)
      this.flushTimer = null
    }
    if (this.buf.length === 0) return
    const b = this.buf
    this.buf = []
    this.h.onDeltas(b)
  }
}

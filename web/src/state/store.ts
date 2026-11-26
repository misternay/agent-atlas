import { create } from 'zustand'
import { api, ApiError } from '../api/client'
import { AtlasStream } from '../api/stream'
import type { Delta } from '../api/types'
import { applyDeltas, applySnapshot, emptyState, type AtlasState } from './reducer'

export type View = 'live' | 'history' | 'settings'

export interface Selection {
  kind: 'session' | 'span' | 'resource'
  id: string
}

interface Store {
  view: View
  live: AtlasState
  livePaused: boolean // UI-level pause: buffer updates, don't render
  pending: Delta[]
  connected: boolean
  collectionPaused: boolean
  authError: boolean
  windowS: number
  harnessFilter: string
  workspaceFilter: string
  search: string
  selectedSession: string | null
  selection: Selection | null
  traceId: string | null
  setView: (v: View) => void
  setFilter: (f: Partial<Pick<Store, 'harnessFilter' | 'workspaceFilter' | 'search' | 'windowS'>>) => void
  select: (s: Selection | null) => void
  selectSession: (id: string | null) => void
  openTrace: (traceId: string | null) => void
  toggleLivePause: () => void
  boot: () => Promise<void>
  resync: () => Promise<void>
  setCollectionPaused: (p: boolean) => Promise<void>
}

let stream: AtlasStream | null = null
let generation = 0
let resyncRequest = 0
let resyncTimer: number | undefined

export const useStore = create<Store>((set, get) => ({
  view: 'live',
  live: emptyState(),
  livePaused: false,
  pending: [],
  connected: false,
  collectionPaused: false,
  authError: false,
  windowS: 60,
  harnessFilter: '',
  workspaceFilter: '',
  search: '',
  selectedSession: null,
  selection: null,
  traceId: null,
  setView: (view) => set({ view }),
  setFilter: (f) => {
    const widen = f.windowS !== undefined && f.windowS !== get().windowS
    set(f)
    // The snapshot only carries spans inside the live window; reload on change.
    if (widen) void get().resync()
  },
  select: (selection) => set({ selection }),
  selectSession: (selectedSession) => set({ selectedSession }),
  openTrace: (traceId) => set({ traceId, view: traceId ? 'history' : get().view }),
  toggleLivePause: () => {
    const { livePaused, pending, live } = get()
    if (livePaused) set({ livePaused: false, pending: [], live: applyDeltas(live, pending) })
    else set({ livePaused: true })
  },
  boot: async () => {
    await get().resync()
    if (get().authError) return
    stream?.stop()
    stream = new AtlasStream({
      onDeltas: (d) => {
        const st = get()
        if (st.livePaused) set({ pending: st.pending.concat(d).slice(-20000) })
        else set({ live: applyDeltas(st.live, d) })
      },
      onResync: () => {
        void get().resync()
      },
      onStatus: (s) => set({ connected: s.connected, ...(s.paused !== undefined ? { collectionPaused: s.paused } : {}) }),
    })
    stream.start(get().live.cursor, generation)
  },
  resync: async () => {
    const request = ++resyncRequest
    window.clearTimeout(resyncTimer)
    try {
      const snap = await api.snapshot(get().windowS, 7 * 86400)
      if (request !== resyncRequest) return
      set({ live: applySnapshot(snap), pending: [], authError: false })
      generation = snap.generation ?? 0
      // Deltas that arrived during the fetch were applied to the old state and
      // are now overwritten; restarting from the snapshot cursor replays them.
      stream?.restart(snap.cursor, generation)
      const h = await api.health()
      if (request === resyncRequest && h.paused !== undefined) set({ collectionPaused: h.paused })
    } catch (e) {
      if (request !== resyncRequest) return
      if (e instanceof ApiError && e.status === 401) {
        stream?.stop()
        set({ authError: true, connected: false })
        return
      }
      // Daemon unreachable: retry the snapshot (and stream) shortly.
      set({ connected: false })
      resyncTimer = window.setTimeout(() => void get().resync(), 3000)
    }
  },
  setCollectionPaused: async (p) => {
    const r = await api.setPaused(p)
    set({ collectionPaused: r.paused })
  },
}))

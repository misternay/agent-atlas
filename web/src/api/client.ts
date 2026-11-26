import type { Capability, Health, Session, Settings, Snapshot, SpanResult, StoredEvent, Span, Link, Delta } from './types'
import { sessionSecret } from './session'

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function get<T>(path: string): Promise<T> {
  const r = await fetch(path, { credentials: 'omit', redirect: 'error', headers: { Accept: 'application/json', Authorization: `AtlasSession ${sessionSecret()}` } })
  if (!r.ok) throw new ApiError(r.status, `${r.status} ${path}`)
  return (await r.json()) as T
}

async function post<T>(path: string, body: unknown): Promise<T> {
  const r = await fetch(path, {
    method: 'POST',
    credentials: 'omit',
    redirect: 'error',
    headers: { 'Content-Type': 'application/json', 'X-Atlas-CSRF': '1', Authorization: `AtlasSession ${sessionSecret()}` },
    body: JSON.stringify(body),
  })
  if (!r.ok) throw new ApiError(r.status, `${r.status} ${path}`)
  return (await r.json()) as T
}

function qs(params: Record<string, string | number | undefined>): string {
  const u = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '') u.set(k, String(v))
  const s = u.toString()
  return s ? `?${s}` : ''
}

export interface SessionQuery {
  status?: string
  harness?: string
  workspace?: string
  q?: string
  from?: number
  to?: number
  limit?: number
  offset?: number
}

export interface SpanQuery {
  tool?: string
  category?: string
  status?: string
  harness?: string
  workspace?: string
  session_id?: string
  resource?: string
  evidence?: string
  from?: number
  to?: number
  limit?: number
  offset?: number
}

export const api = {
  health: () => get<Health>('/api/v1/health'),
  snapshot: (windowS = 60, sessionsSinceS = 86400) => get<Snapshot>(`/api/v1/snapshot${qs({ window_s: windowS, sessions_since_s: sessionsSinceS })}`),
  sessions: (q: SessionQuery = {}) => get<{ sessions: Session[]; total: number }>(`/api/v1/sessions${qs({ ...q })}`),
  session: (id: string) => get<Session>(`/api/v1/sessions/${encodeURIComponent(id)}`),
  sessionEvents: (id: string, afterSeq = 0, limit = 500) =>
    get<{ events: StoredEvent[]; next_after_seq: number; min_seq: number; max_seq: number }>(
      `/api/v1/sessions/${encodeURIComponent(id)}/events${qs({ after_seq: afterSeq, limit })}`,
    ),
  replay: (id: string) => get<{ session: Session; frames: Delta[]; executes_tools: false; truncated: boolean }>(`/api/v1/sessions/${encodeURIComponent(id)}/replay`),
  trace: (traceId: string) => get<{ session: Session; spans: Span[]; links: Link[] }>(`/api/v1/traces/${encodeURIComponent(traceId)}`),
  traceOTLP: (traceId: string) => get<unknown>(`/api/v1/traces/${encodeURIComponent(traceId)}?format=otlp`),
  search: (q: SpanQuery) => get<{ spans: SpanResult[]; total: number }>(`/api/v1/search${qs({ ...q })}`),
  settings: () => get<Settings>('/api/v1/settings'),
  capabilities: () => get<{ harnesses: Capability[] }>('/api/v1/capabilities'),
  setPaused: (paused: boolean) => post<{ paused: boolean }>('/api/v1/admin/pause', { paused }),
  clearHistory: () => post<{ cleared: boolean }>('/api/v1/admin/clear-history', { confirm: 'delete all history' }),
}

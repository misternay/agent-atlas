export function fmtDuration(us?: number | null): string {
  if (us === undefined || us === null) return '—'
  if (us < 1000) return `${us}µs`
  const ms = us / 1000
  if (ms < 1000) return `${ms.toFixed(ms < 10 ? 1 : 0)}ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(s < 10 ? 2 : 1)}s`
  const m = Math.floor(s / 60)
  return `${m}m${Math.round(s % 60)}s`
}

export function fmtTime(us?: number | null): string {
  if (!us) return '—'
  const d = new Date(us / 1000)
  return d.toLocaleTimeString(undefined, { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0')
}

export function fmtDateTime(us?: number | null): string {
  if (!us) return '—'
  return new Date(us / 1000).toLocaleString(undefined, { hour12: false })
}

export function fmtAgo(us: number, nowUs: number): string {
  const s = Math.max(0, Math.round((nowUs - us) / 1e6))
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export function fmtBytes(n?: number): string {
  if (!n) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${u[i]}`
}

export function shortId(id?: string, n = 8): string {
  if (!id) return ''
  return id.length > n ? id.slice(0, n) : id
}

export const nowUs = () => Date.now() * 1000

export const evidenceHelp: Record<string, string> = {
  observed: 'Observed: a native hook event directly reported this.',
  derived: 'Derived: deterministically computed from observed fields (e.g. duration, exit code, session end).',
  inferred: 'Inferred: heuristic (e.g. a file operand parsed from a shell command or a denial recognized from result text). Not verified.',
  unknown: 'Unknown: insufficient evidence.',
}

export const statusHelp: Record<string, string> = {
  running: 'Start received, no terminal event yet.',
  completed: 'Terminal success received.',
  failed: 'Explicit failure signal received.',
  denied: 'Denial received or recognized.',
  unknown: 'Terminal outcome could not be established (e.g. session ended before completion).',
  stale: 'Expected end is missing past the threshold. Not the same as failure; a late completion can still arrive.',
}

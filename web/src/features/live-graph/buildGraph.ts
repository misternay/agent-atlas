// Pure projection of client state into Agent/Session → Tool → Resource graph
// nodes and edges with live window, filtering, and grouping (design §8.3).
import type { Evidence, Link, Session, Span } from '../../api/types'
import { linksOf, sessionLiveness, type AtlasState } from '../../state/reducer'

export type GNodeType = 'session' | 'tool' | 'resource' | 'group'

export interface GNode {
  id: string
  type: GNodeType
  label: string
  sub?: string
  status?: string
  evidence?: Evidence
  sessionId?: string
  spanId?: string
  resourceKind?: string
  count?: number
  groupKey?: string
}

export interface GEdge {
  id: string
  source: string
  target: string
  relation: string
  evidence: Evidence
}

export interface GraphOptions {
  nowUs: number
  windowS: number
  harness?: string
  workspace?: string
  search?: string
  selectedSession?: string | null
  maxToolsPerSession?: number
  maxResourcesPerTool?: number
  expanded?: Set<string>
  showEndedForS?: number
}

const isOpen = (s: Span) => s.status === 'running' || s.status === 'stale'

function matchesSearch(q: string, sp: Span, links: Link[]): boolean {
  if (!q) return true
  const t = q.toLowerCase()
  if (sp.name.toLowerCase().includes(t)) return true
  if (sp.target_label?.toLowerCase().includes(t)) return true
  return links.some((l) => l.label.toLowerCase().includes(t))
}

export function visibleSessions(state: AtlasState, o: GraphOptions): Session[] {
  const showEnded = (o.showEndedForS ?? 600) * 1e6
  return Object.values(state.sessions)
    .filter((s) => {
      if (o.selectedSession) return s.id === o.selectedSession
      if (o.harness && s.harness !== o.harness) return false
      if (o.workspace && !s.workspace.toLowerCase().includes(o.workspace.toLowerCase())) return false
      const live = sessionLiveness(s, o.nowUs)
      if (live === 'ended') return o.nowUs - s.last_event_at_us < showEnded
      return true
    })
    .sort((a, b) => b.last_event_at_us - a.last_event_at_us)
}

export function buildGraph(state: AtlasState, o: GraphOptions): { nodes: GNode[]; edges: GEdge[]; hiddenTools: number } {
  const maxTools = o.maxToolsPerSession ?? 24
  const maxRes = o.maxResourcesPerTool ?? 6
  const windowStart = o.nowUs - o.windowS * 1e6
  const sessions = visibleSessions(state, o)
  const nodes: GNode[] = []
  const edges: GEdge[] = []
  const resNodes = new Map<string, GNode>()
  let hiddenTools = 0
  const bySession = new Map<string, Span[]>()
  for (const sp of Object.values(state.spans)) {
    if (sp.kind !== 'tool') continue
    const inWindow = isOpen(sp) || (sp.ended_at_us ?? sp.started_at_us) >= windowStart || (o.selectedSession != null && sp.session_id === o.selectedSession)
    if (!inWindow) continue
    const arr = bySession.get(sp.session_id) ?? []
    arr.push(sp)
    bySession.set(sp.session_id, arr)
  }
  for (const s of sessions) {
    const sid = `session:${s.id}`
    nodes.push({ id: sid, type: 'session', label: s.harness === 'claude' ? 'Claude Code' : s.harness === 'codex' ? 'Codex' : s.harness, sub: s.workspace, status: sessionLiveness(s, o.nowUs), sessionId: s.id })
    const spans = (bySession.get(s.id) ?? [])
      .filter((sp) => matchesSearch(o.search ?? '', sp, linksOf(state, sp.id)))
      .sort((a, b) => Number(isOpen(b)) - Number(isOpen(a)) || b.started_at_us - a.started_at_us)
    const groupKey = `tools:${s.id}`
    const expandedTools = o.expanded?.has(groupKey)
    let folded = 0
    spans.forEach((sp, i) => {
      if (!expandedTools && i >= maxTools && !isOpen(sp)) {
        folded++
        return
      }
      const tid = `tool:${sp.id}`
      nodes.push({ id: tid, type: 'tool', label: sp.name, sub: sp.target_label, status: sp.status, evidence: sp.status_evidence, sessionId: s.id, spanId: sp.id })
      edges.push({ id: `e:${sid}:${tid}`, source: sid, target: tid, relation: 'invokes', evidence: sp.status_evidence })
      const links = linksOf(state, sp.id).filter((l) => !(l.kind === 'process' && l.relation === 'invokes' && l.label === sp.target_label))
      const resKey = `res:${sp.id}`
      const expandedRes = o.expanded?.has(resKey)
      links.forEach((l, j) => {
        if (!expandedRes && j >= maxRes) return
        const rid = `res:${l.resource_id}`
        if (!resNodes.has(rid)) {
          const n: GNode = { id: rid, type: 'resource', label: l.label, resourceKind: l.kind, evidence: l.evidence }
          resNodes.set(rid, n)
        } else {
          const n = resNodes.get(rid)!
          const r: Record<string, number> = { observed: 3, derived: 2, inferred: 1, unknown: 0 }
          if ((r[l.evidence] ?? 0) > (r[n.evidence ?? 'unknown'] ?? 0)) n.evidence = l.evidence
        }
        edges.push({ id: `e:${tid}:${rid}:${l.relation}`, source: tid, target: rid, relation: l.relation, evidence: l.evidence })
      })
      if (!expandedRes && links.length > maxRes) {
        const gid = `group:${resKey}`
        nodes.push({ id: gid, type: 'group', label: `+${links.length - maxRes} more resources`, count: links.length - maxRes, groupKey: resKey })
        edges.push({ id: `e:${tid}:${gid}`, source: tid, target: gid, relation: 'belongs_to', evidence: 'derived' })
      }
    })
    if (folded > 0) {
      hiddenTools += folded
      const gid = `group:${groupKey}`
      nodes.push({ id: gid, type: 'group', label: `+${folded} earlier calls`, count: folded, groupKey, sessionId: s.id })
      edges.push({ id: `e:${sid}:${gid}`, source: sid, target: gid, relation: 'belongs_to', evidence: 'derived' })
    }
  }
  nodes.push(...resNodes.values())
  return { nodes, edges, hiddenTools }
}

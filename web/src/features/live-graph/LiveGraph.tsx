import { Background, Controls, MiniMap, ReactFlow, type Edge, type Node, type ReactFlowInstance } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useStore } from '../../state/store'
import { buildGraph, type GNode } from './buildGraph'
import { layoutGraph, NODE_SIZE, type Positions } from './layout'
import { nodeTypes } from './nodes'
import { nowUs } from '../../format'

const edgeStyle: Record<string, React.CSSProperties> = {
  observed: { strokeWidth: 1.6 },
  derived: { strokeWidth: 1.4, strokeDasharray: '6 3' },
  inferred: { strokeWidth: 1.2, strokeDasharray: '2 4' },
  unknown: { strokeWidth: 1, strokeDasharray: '1 6' },
}

export function LiveGraph() {
  const live = useStore((s) => s.live)
  const windowS = useStore((s) => s.windowS)
  const harness = useStore((s) => s.harnessFilter)
  const workspace = useStore((s) => s.workspaceFilter)
  const search = useStore((s) => s.search)
  const selectedSession = useStore((s) => s.selectedSession)
  const selection = useStore((s) => s.selection)
  const select = useStore((s) => s.select)
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [tick, setTick] = useState(0)
  const [positions, setPositions] = useState<Positions>({})
  const layoutSeq = useRef(0)
  const lastLayout = useRef(0)
  const pendingTimer = useRef<number | null>(null)
  const flow = useRef<ReactFlowInstance | null>(null)

  // Re-evaluate the live window every 2s even without new events.
  useEffect(() => {
    const t = window.setInterval(() => setTick((x) => x + 1), 2000)
    return () => window.clearInterval(t)
  }, [])

  const graph = useMemo(
    () => buildGraph(live, { nowUs: nowUs(), windowS, harness, workspace, search, selectedSession, expanded }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [live, windowS, harness, workspace, search, selectedSession, expanded, tick],
  )
  const structureKey = useMemo(() => JSON.stringify([graph.nodes.map((n) => n.id), graph.edges.map((e) => e.id)]), [graph])

  // Throttled ELK layout: recompute at most every 400ms, only when structure changes.
  useEffect(() => {
    const seq = ++layoutSeq.current
    const run = () => {
      lastLayout.current = Date.now()
      void layoutGraph(graph.nodes, graph.edges).then((p) => {
        if (seq !== layoutSeq.current) return
        setPositions(p)
        // Re-fit when the set of nodes changed (not on every status update).
        window.requestAnimationFrame(() => flow.current?.fitView({ padding: 0.15, duration: 250, maxZoom: 1.2 }))
      }).catch(() => {
        if (seq !== layoutSeq.current) return
        const rows: Record<string, number> = {}
        const columns = { session: 0, tool: 1, group: 1, resource: 2 }
        setPositions(Object.fromEntries(graph.nodes.map((n) => {
          const column = columns[n.type]
          const row = rows[column] ?? 0
          rows[column] = row + 1
          return [n.id, { x: column * 330, y: row * 75 }]
        })))
      })
    }
    const since = Date.now() - lastLayout.current
    if (pendingTimer.current) window.clearTimeout(pendingTimer.current)
    if (since > 400) run()
    else pendingTimer.current = window.setTimeout(run, 400 - since)
    return () => {
      layoutSeq.current++
      if (pendingTimer.current) window.clearTimeout(pendingTimer.current)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [structureKey])

  const nodes: Node[] = useMemo(
    () =>
      graph.nodes.map((g) => ({
        id: g.id,
        type: g.type,
        position: positions[g.id] ?? { x: 0, y: 0 },
        data: { g },
        // Fixed size = the size ELK laid out; content ellipsizes inside.
        width: NODE_SIZE[g.type].w,
        height: NODE_SIZE[g.type].h,
        selected: !!selection && ((selection.kind === 'span' && g.spanId === selection.id) || (selection.kind === 'session' && g.type === 'session' && g.sessionId === selection.id) || (selection.kind === 'resource' && g.id === 'res:' + selection.id)),
        style: positions[g.id] ? undefined : { opacity: 0 },
      })),
    [graph, positions, selection],
  )
  const edges: Edge[] = useMemo(
    () =>
      graph.edges.map((e) => ({
        id: e.id,
        source: e.source,
        target: e.target,
        label: e.relation === 'invokes' || e.relation === 'belongs_to' ? undefined : e.relation,
        className: `edge-${e.evidence} rel-${e.relation}`,
        style: edgeStyle[e.evidence],
        animated: false,
        data: { evidence: e.evidence },
      })),
    [graph],
  )

  const onNodeClick = (_: unknown, n: Node) => {
    const g = (n.data as { g: GNode }).g
    if (g.type === 'group' && g.groupKey) {
      setExpanded((s) => new Set(s).add(g.groupKey!))
      return
    }
    if (g.type === 'tool' && g.spanId) select({ kind: 'span', id: g.spanId })
    else if (g.type === 'session' && g.sessionId) select({ kind: 'session', id: g.sessionId })
    else if (g.type === 'resource') select({ kind: 'resource', id: g.id.replace(/^res:/, '') })
  }

  return (
    <div className="graph-wrap">
      {graph.nodes.length === 0 ? (
        <div className="empty">
          <h3>No live activity</h3>
          <p>Sessions appear here when a hooked Claude Code or Codex session runs. Running tool calls stay visible; finished calls show for the last {windowS}s.</p>
          <p className="muted">Not seeing anything? Run <code>atlas doctor</code>.</p>
        </div>
      ) : (
        <ReactFlow
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          onNodeClick={onNodeClick}
          onPaneClick={() => select(null)}
          onInit={(inst) => (flow.current = inst)}
          fitView
          fitViewOptions={{ padding: 0.15 }}
          minZoom={0.1}
          proOptions={{ hideAttribution: true }}
          nodesDraggable={false}
          nodesConnectable={false}
        >
          <Background gap={24} size={1} />
          <Controls showInteractive={false} />
          <MiniMap pannable zoomable className="minimap" />
        </ReactFlow>
      )}
      {graph.hiddenTools > 0 && <div className="graph-note">{graph.hiddenTools} older calls grouped — click a group node to expand</div>}
      <div className="legend">
        <span><i className="lg observed" /> observed</span>
        <span><i className="lg derived" /> derived</span>
        <span><i className="lg inferred" /> inferred</span>
      </div>
    </div>
  )
}

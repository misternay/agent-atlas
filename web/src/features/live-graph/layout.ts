// ELK.js layered layout (design §3: ELK.js for layered graph layout).
import type { GEdge, GNode } from './buildGraph'

let engine: Promise<InstanceType<typeof import('elkjs/lib/elk.bundled.js').default>> | undefined

export const NODE_SIZE: Record<string, { w: number; h: number }> = {
  session: { w: 210, h: 58 },
  tool: { w: 230, h: 54 },
  resource: { w: 210, h: 40 },
  group: { w: 170, h: 36 },
}

export type Positions = Record<string, { x: number; y: number }>

export async function layoutGraph(nodes: GNode[], edges: GEdge[]): Promise<Positions> {
  if (nodes.length === 0) return {}
  engine ??= import('elkjs/lib/elk.bundled.js').then(({ default: ELK }) => new ELK())
  const ids = new Set(nodes.map((n) => n.id))
  const graph = {
    id: 'root',
    layoutOptions: {
      'elk.algorithm': 'layered',
      'elk.direction': 'RIGHT',
      'elk.layered.spacing.nodeNodeBetweenLayers': '90',
      'elk.spacing.nodeNode': '18',
      'elk.layered.nodePlacement.strategy': 'BRANDES_KOEPF',
      'elk.layered.crossingMinimization.strategy': 'LAYER_SWEEP',
      'elk.separateConnectedComponents': 'true',
      'elk.spacing.componentComponent': '40',
    },
    children: nodes.map((n) => ({ id: n.id, width: NODE_SIZE[n.type].w, height: NODE_SIZE[n.type].h })),
    edges: edges.filter((e) => ids.has(e.source) && ids.has(e.target)).map((e) => ({ id: e.id, sources: [e.source], targets: [e.target] })),
  }
  const res = await (await engine).layout(graph)
  const pos: Positions = {}
  for (const c of res.children ?? []) pos[c.id] = { x: c.x ?? 0, y: c.y ?? 0 }
  return pos
}

import { BaseEdge, type EdgeProps } from '@xyflow/react'
import type { GEdge } from './buildGraph'

export function parallelOffsets(edges: GEdge[]): Map<string, number> {
  const byEndpoints = new Map<string, string[]>()
  for (const { id, source, target } of edges) {
    const key = JSON.stringify([source, target])
    const group = byEndpoints.get(key) ?? []
    group.push(id)
    byEndpoints.set(key, group)
  }

  const offsets = new Map<string, number>()
  for (const group of byEndpoints.values()) {
    if (group.length < 2) continue
    const middle = (group.length - 1) / 2
    group.sort().forEach((id, index) => offsets.set(id, (index - middle) * 28))
  }
  return offsets
}

function ParallelEdge({ id, sourceX, sourceY, targetX, targetY, style, label, data }: EdgeProps) {
  const offset = Number(data?.offset ?? 0)
  // All graph nodes have right-hand source and left-hand target handles.
  // Shift both control points so the curves share endpoints and separate at their midpoint.
  const reach = Math.max(24, Math.abs(targetX - sourceX) / 2)
  const bend = offset * 4 / 3
  const path = `M ${sourceX},${sourceY} C ${sourceX + reach},${sourceY + bend} ${targetX - reach},${targetY + bend} ${targetX},${targetY}`
  return <BaseEdge id={id} path={path} style={style} label={label} labelX={(sourceX + targetX) / 2} labelY={(sourceY + targetY) / 2 + offset} />
}

export const edgeTypes = { parallel: ParallelEdge }

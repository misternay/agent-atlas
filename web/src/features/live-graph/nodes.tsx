import { Handle, Position, type NodeProps } from '@xyflow/react'
import { memo } from 'react'
import type { GNode } from './buildGraph'

type Data = { g: GNode; selected?: boolean }

const kindIcon: Record<string, string> = { file: '📄', mcp: '🔌', process: '⚙︎', network: '🌐' }

function Frame({ g, children, cls }: { g: GNode; children: React.ReactNode; cls: string }) {
  return (
    <div className={`gnode ${cls} ${g.status ? 'st-' + g.status : ''}`}>
      <Handle type="target" position={Position.Left} className="h" />
      {children}
      <Handle type="source" position={Position.Right} className="h" />
    </div>
  )
}

export const SessionNode = memo(({ data }: NodeProps) => {
  const { g } = data as Data
  return (
    <Frame g={g} cls={`gn-session harness-${g.label === 'Codex' ? 'codex' : 'claude'}`}>
      <div className="gn-title">{g.label}</div>
      <div className="gn-sub" title={g.sub}>{g.sub}</div>
      <div className={`gn-live live-${g.status}`}>{g.status}</div>
    </Frame>
  )
})

export const ToolNode = memo(({ data }: NodeProps) => {
  const { g } = data as Data
  return (
    <Frame g={g} cls="gn-tool">
      <div className="gn-title">
        {g.status === 'running' && <span className="pulse" aria-hidden />}
        {g.label}
        <span className={`gn-status status-${g.status}`}>{g.status}</span>
      </div>
      {g.sub && <div className="gn-sub" title={g.sub}>{g.sub}</div>}
    </Frame>
  )
})

export const ResourceNode = memo(({ data }: NodeProps) => {
  const { g } = data as Data
  return (
    <Frame g={g} cls={`gn-resource evidence-${g.evidence}`}>
      <div className="gn-title" title={`${g.resourceKind}: ${g.label} (${g.evidence})`}>
        <span className="gn-icon">{kindIcon[g.resourceKind ?? ''] ?? '•'}</span>
        <span className="gn-label">{g.label}</span>
      </div>
    </Frame>
  )
})

export const GroupNode = memo(({ data }: NodeProps) => {
  const { g } = data as Data
  return (
    <Frame g={g} cls="gn-group">
      <div className="gn-title">{g.label}</div>
      <div className="gn-sub">click to expand</div>
    </Frame>
  )
})

export const nodeTypes = { session: SessionNode, tool: ToolNode, resource: ResourceNode, group: GroupNode }

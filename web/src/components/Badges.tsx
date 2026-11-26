import { evidenceHelp, statusHelp } from '../format'

export function StatusBadge({ status }: { status: string }) {
  return (
    <span className={`badge status-${status}`} title={statusHelp[status] ?? status}>
      {status === 'running' && <span className="pulse" aria-hidden />}
      {status}
    </span>
  )
}

export function EvidenceBadge({ evidence }: { evidence: string }) {
  return (
    <span className={`badge evidence-${evidence}`} title={evidenceHelp[evidence] ?? evidence}>
      {evidence}
    </span>
  )
}

export function HarnessBadge({ harness }: { harness: string }) {
  return <span className={`badge harness-${harness}`}>{harness === 'claude' ? 'Claude Code' : harness === 'codex' ? 'Codex' : harness}</span>
}

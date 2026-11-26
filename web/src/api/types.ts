// Wire types mirroring internal/correlate, internal/event and internal/api (Go).
// All *_us fields are UTC epoch microseconds.

export type Harness = 'claude' | 'codex' | 'atlas'
export type Status = 'running' | 'completed' | 'failed' | 'denied' | 'unknown' | 'stale'
export type Evidence = 'observed' | 'derived' | 'inferred' | 'unknown'
export type SpanKind = 'session' | 'turn' | 'tool'
export type Relation = 'reads' | 'writes' | 'connects_to' | 'invokes' | 'belongs_to'

export interface Session {
  id: string
  host_id: string
  harness: Harness
  source_session_id: string
  workspace: string
  status: 'active' | 'ended'
  start_observed: boolean
  started_at_us: number
  ended_at_us?: number
  last_event_at_us: number
  trace_id: string
  root_span_id: string
  tool_count: number
  error_count: number
  denied_count: number
  unknown_count: number
  model?: string
  attrs?: Record<string, string>
}

export interface Span {
  id: string
  trace_id: string
  session_id: string
  parent_span_id?: string
  kind: SpanKind
  name: string
  tool_category?: string
  operation?: string
  target_kind?: string
  target_label?: string
  tool_call_id?: string
  turn_id?: string
  status: Status
  status_evidence: Evidence
  outcome_basis?: string
  started_at_us: number
  ended_at_us?: number
  duration_us?: number
  start_seq?: number
  end_seq?: number
  last_seq: number
  attrs?: Record<string, string>
}

export interface Link {
  span_id: string
  resource_id: string
  kind: string
  label: string
  relation: Relation
  evidence: Evidence
  event_seq: number
}

export interface AtlasEvent {
  schema_version: number
  event_id: string
  timestamp: string
  host_id: string
  harness: Harness
  session_id?: string
  turn_id?: string
  tool_call_id?: string
  agent_id?: string
  type: string
  source_event?: string
  workspace?: string
  tool?: { name: string; category?: string; operation?: string; mcp_server?: string; mcp_tool?: string }
  target?: { kind: string; label: string }
  resources?: { kind: string; label: string; relation: Relation; evidence: Evidence }[]
  status?: Status
  evidence: Evidence
  privacy: string
  duration_ms?: number
  attrs?: Record<string, string>
}

export interface StoredEvent {
  seq: number
  session_id?: string
  span_id?: string
  event: AtlasEvent
}

export interface Delta {
  seq: number
  event: AtlasEvent
  session?: Session
  spans?: Span[]
  links?: Link[]
  span_id?: string
}

export interface Snapshot {
  cursor: number
  generation?: number
  sessions: Session[]
  spans: Span[]
  links: Link[]
}

export interface StreamMessage {
  type: 'hello' | 'delta' | 'resync_required' | 'status'
  seq?: number
  cursor?: number
  reason?: string
  paused?: boolean
  event?: AtlasEvent
  session?: Session
  spans?: Span[]
  links?: Link[]
  replay?: boolean
}

export interface SpanResult extends Span {
  harness: Harness
  workspace: string
}

export interface Health {
  ok: boolean
  version?: string
  paused?: boolean
  uptime_s?: number
  ingest?: Record<string, number>
  storage?: Record<string, number>
  spool_bytes?: number
  export_enabled?: boolean
}

export interface Settings {
  paused: boolean
  addr: string
  retention_days: number
  max_db_bytes: number
  spool_max_bytes: number
  stale_after_secs: number
  export_enabled: boolean
  raw_prompts: boolean
  raw_tool_output: boolean
  home: string
  stored_fields: string[]
  never_stored: string[]
  retention?: Record<string, unknown> | null
}

export interface Capability {
  harness: Harness
  capabilities: {
    session_lifecycle: boolean
    turn_lifecycle: boolean
    tool_start: boolean
    tool_finish: boolean
    tool_failure: boolean
    tool_denied: boolean
    subagents: boolean
    mcp_calls: boolean
    notes?: string[]
    hook_events: string[]
    payload_basis: string
  }
  observed_hooks?: Record<string, number>
  not_yet_observed?: string[]
}

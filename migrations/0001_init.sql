-- Agent Atlas schema v1 (design §7). All timestamps are UTC epoch microseconds.

CREATE TABLE sessions (
    id                TEXT PRIMARY KEY,           -- s_<hash(host|harness|source_session_id)>
    host_id           TEXT NOT NULL,
    harness           TEXT NOT NULL,
    source_session_id TEXT NOT NULL,
    workspace_path    TEXT NOT NULL DEFAULT '',  -- home-collapsed, sanitized
    status            TEXT NOT NULL,              -- active | ended
    start_observed    INTEGER NOT NULL DEFAULT 0, -- 0 = placeholder created before any start signal
    started_at        INTEGER NOT NULL,
    ended_at          INTEGER,
    last_event_at     INTEGER NOT NULL,
    trace_id          TEXT NOT NULL,
    root_span_id      TEXT NOT NULL,
    tool_count        INTEGER NOT NULL DEFAULT 0,
    error_count       INTEGER NOT NULL DEFAULT 0,
    denied_count      INTEGER NOT NULL DEFAULT 0,
    unknown_count     INTEGER NOT NULL DEFAULT 0,
    model             TEXT NOT NULL DEFAULT '',
    attrs_json        TEXT NOT NULL DEFAULT '{}',
    UNIQUE (host_id, harness, source_session_id)
);
CREATE INDEX sessions_last_event ON sessions (last_event_at DESC);
CREATE INDEX sessions_harness ON sessions (harness, last_event_at DESC);
CREATE INDEX sessions_workspace ON sessions (workspace_path, last_event_at DESC);

CREATE TABLE events (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT, -- durable, never-reused replay cursor
    event_id      TEXT NOT NULL UNIQUE,
    session_id    TEXT REFERENCES sessions (id) ON DELETE CASCADE,
    type          TEXT NOT NULL,
    harness       TEXT NOT NULL,
    occurred_at   INTEGER NOT NULL,
    tool_call_id  TEXT NOT NULL DEFAULT '',
    span_id       TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL                       -- sanitized event JSON
);
CREATE INDEX events_session_seq ON events (session_id, seq);
CREATE INDEX events_type_time ON events (type, occurred_at);
CREATE INDEX events_time ON events (occurred_at);

CREATE TABLE spans (
    id              TEXT PRIMARY KEY,
    trace_id        TEXT NOT NULL,
    session_id      TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    parent_span_id  TEXT NOT NULL DEFAULT '',  -- only when grounded in source ids
    kind            TEXT NOT NULL,             -- session | turn | tool
    name            TEXT NOT NULL,
    tool_category   TEXT NOT NULL DEFAULT '',
    operation       TEXT NOT NULL DEFAULT '',
    target_kind     TEXT NOT NULL DEFAULT '',
    target_label    TEXT NOT NULL DEFAULT '',
    tool_call_id    TEXT NOT NULL DEFAULT '',
    turn_id         TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL,
    status_evidence TEXT NOT NULL,
    outcome_basis   TEXT NOT NULL DEFAULT '',
    started_at      INTEGER NOT NULL,
    ended_at        INTEGER,
    duration_us     INTEGER,
    start_seq       INTEGER NOT NULL DEFAULT 0,
    end_seq         INTEGER NOT NULL DEFAULT 0,
    last_seq        INTEGER NOT NULL,
    attrs_json      TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX spans_trace_started ON spans (trace_id, started_at);
CREATE INDEX spans_session_started ON spans (session_id, started_at);
CREATE INDEX spans_status_started ON spans (status, started_at);
CREATE INDEX spans_name_started ON spans (name, started_at);
CREATE INDEX spans_started ON spans (started_at);

CREATE TABLE resources (
    id    TEXT PRIMARY KEY,
    kind  TEXT NOT NULL,
    label TEXT NOT NULL,
    UNIQUE (kind, label)
);
CREATE INDEX resources_kind_label ON resources (kind, label);

CREATE TABLE span_resources (
    span_id     TEXT NOT NULL REFERENCES spans (id) ON DELETE CASCADE,
    resource_id TEXT NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
    relation    TEXT NOT NULL,
    evidence    TEXT NOT NULL,
    event_seq   INTEGER NOT NULL,  -- source event of the (strongest) evidence
    PRIMARY KEY (span_id, resource_id, relation)
);
CREATE INDEX span_resources_resource ON span_resources (resource_id);

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

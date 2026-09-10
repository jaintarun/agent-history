PRAGMA defer_foreign_keys = ON;

CREATE TABLE sessions_new (
    id TEXT PRIMARY KEY,
    agent TEXT NOT NULL CHECK (agent IN ('codex', 'claude', 'grok')),
    native_session_id TEXT NOT NULL,
    source_path TEXT NOT NULL,
    source_size INTEGER NOT NULL DEFAULT 0 CHECK (source_size >= 0),
    source_mtime TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    working_directory TEXT NOT NULL,
    title TEXT,
    summary TEXT,
    topic_count INTEGER CHECK (topic_count >= 0),
    started_at TEXT NOT NULL,
    last_active_at TEXT NOT NULL,
    analysis_status TEXT NOT NULL DEFAULT 'none'
        CHECK (analysis_status IN ('none', 'queued', 'running', 'current', 'partial', 'failed')),
    analysis_error TEXT,
    analysis_provider TEXT,
    analysis_model TEXT,
    analysis_prompt_version TEXT,
    analyzed_at TEXT,
    analyzed_hash TEXT,
    analyzed_through_sequence INTEGER,
    analyzed_through_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (agent, native_session_id)
);

CREATE TABLE messages_new (
    session_id TEXT NOT NULL REFERENCES sessions_new(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    timestamp TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
    text TEXT NOT NULL,
    tool_name TEXT,
    PRIMARY KEY (session_id, sequence)
);

CREATE TABLE segments_new (
    session_id TEXT NOT NULL REFERENCES sessions_new(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    start_sequence INTEGER NOT NULL CHECK (start_sequence >= 0),
    end_sequence INTEGER NOT NULL CHECK (end_sequence >= start_sequence),
    title TEXT NOT NULL,
    summary TEXT NOT NULL,
    detail TEXT NOT NULL,
    PRIMARY KEY (session_id, position)
);

CREATE TABLE summary_nodes_new (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions_new(id) ON DELETE CASCADE,
    parent_id TEXT REFERENCES summary_nodes_new(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    kind TEXT NOT NULL CHECK (kind IN ('leaf', 'rollup', 'topic', 'session')),
    position INTEGER NOT NULL CHECK (position >= 0),
    start_sequence INTEGER NOT NULL CHECK (start_sequence >= 0),
    end_sequence INTEGER NOT NULL CHECK (end_sequence >= start_sequence),
    input_hash TEXT NOT NULL,
    summary_json TEXT NOT NULL,
    sealed INTEGER NOT NULL CHECK (sealed IN (0, 1)),
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    normalizer_version TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE cmux_session_state_new (
    session_id TEXT PRIMARY KEY REFERENCES sessions_new(id) ON DELETE CASCADE,
    open INTEGER NOT NULL DEFAULT 0 CHECK (open IN (0, 1)),
    workspace_id TEXT NOT NULL DEFAULT '',
    surface_id TEXT NOT NULL DEFAULT '',
    workspace_title TEXT NOT NULL DEFAULT '',
    surface_title TEXT NOT NULL DEFAULT '',
    workspace_has_custom_title INTEGER NOT NULL DEFAULT 0
        CHECK (workspace_has_custom_title IN (0, 1)),
    lifecycle TEXT NOT NULL DEFAULT 'unknown'
        CHECK (lifecycle IN ('running', 'idle', 'needsInput', 'unknown')),
    observed_at TEXT NOT NULL,
    last_pushed_workspace_title TEXT,
    last_pushed_surface_title TEXT,
    last_pushed_at TEXT
);

INSERT INTO sessions_new SELECT * FROM sessions;
INSERT INTO messages_new SELECT * FROM messages;
INSERT INTO segments_new SELECT * FROM segments;
INSERT INTO summary_nodes_new SELECT * FROM summary_nodes;
INSERT INTO cmux_session_state_new SELECT * FROM cmux_session_state;

DROP TABLE cmux_session_state;
DROP TABLE summary_nodes;
DROP TABLE segments;
DROP TABLE messages;
DROP TABLE sessions;

ALTER TABLE sessions_new RENAME TO sessions;
ALTER TABLE messages_new RENAME TO messages;
ALTER TABLE segments_new RENAME TO segments;
ALTER TABLE summary_nodes_new RENAME TO summary_nodes;
ALTER TABLE cmux_session_state_new RENAME TO cmux_session_state;

CREATE INDEX sessions_last_active_idx ON sessions(last_active_at DESC, id);
CREATE INDEX sessions_started_idx ON sessions(started_at DESC, id);
CREATE INDEX sessions_cwd_idx ON sessions(working_directory);
CREATE UNIQUE INDEX summary_nodes_cache_idx ON summary_nodes(
    session_id, kind, start_sequence, end_sequence, input_hash,
    provider, model, prompt_version, normalizer_version
);
CREATE INDEX summary_nodes_parent_idx ON summary_nodes(parent_id);
CREATE INDEX summary_nodes_session_range_idx ON summary_nodes(session_id, start_sequence, end_sequence);
CREATE INDEX cmux_session_state_open_idx ON cmux_session_state(open, session_id);

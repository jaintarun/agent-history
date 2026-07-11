CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    agent TEXT NOT NULL CHECK (agent IN ('codex', 'claude')),
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

CREATE INDEX sessions_last_active_idx ON sessions(last_active_at DESC, id);
CREATE INDEX sessions_started_idx ON sessions(started_at DESC, id);
CREATE INDEX sessions_cwd_idx ON sessions(working_directory);

CREATE TABLE messages (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    timestamp TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
    text TEXT NOT NULL,
    tool_name TEXT,
    PRIMARY KEY (session_id, sequence)
);

CREATE TABLE segments (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    start_sequence INTEGER NOT NULL CHECK (start_sequence >= 0),
    end_sequence INTEGER NOT NULL CHECK (end_sequence >= start_sequence),
    title TEXT NOT NULL,
    summary TEXT NOT NULL,
    detail TEXT NOT NULL,
    PRIMARY KEY (session_id, position)
);

CREATE TABLE summary_nodes (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    parent_id TEXT REFERENCES summary_nodes(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
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

CREATE UNIQUE INDEX summary_nodes_cache_idx ON summary_nodes(
    session_id, kind, start_sequence, end_sequence, input_hash,
    provider, model, prompt_version, normalizer_version
);
CREATE INDEX summary_nodes_parent_idx ON summary_nodes(parent_id);
CREATE INDEX summary_nodes_session_range_idx ON summary_nodes(session_id, start_sequence, end_sequence);

CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE VIRTUAL TABLE session_fts USING fts5(
    session_id UNINDEXED,
    document_key UNINDEXED,
    document_type UNINDEXED,
    title,
    body,
    working_directory,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TABLE cmux_status (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    available INTEGER NOT NULL CHECK (available IN (0, 1)),
    access_mode TEXT NOT NULL,
    error TEXT NOT NULL,
    observed_at TEXT NOT NULL
);

CREATE TABLE cmux_session_state (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
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

CREATE INDEX cmux_session_state_open_idx ON cmux_session_state(open, session_id);

ALTER TABLE cmux_session_state
ADD COLUMN workspace_description TEXT NOT NULL DEFAULT '';

ALTER TABLE cmux_session_state
ADD COLUMN last_pushed_workspace_description TEXT;

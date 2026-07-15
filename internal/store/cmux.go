package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CmuxStatus is the most recent result of contacting the local cmux API.
type CmuxStatus struct {
	Available  bool
	AccessMode string
	Error      string
	ObservedAt time.Time
}

// CmuxSessionState is the last observed cmux state for one imported session.
type CmuxSessionState struct {
	SessionID                string
	Open                     bool
	WorkspaceID              string
	SurfaceID                string
	WorkspaceTitle           string
	SurfaceTitle             string
	WorkspaceHasCustomTitle  bool
	Lifecycle                string
	ObservedAt               time.Time
	LastPushedWorkspaceTitle string
	LastPushedSurfaceTitle   string
	LastPushedAt             time.Time
}

// SessionIdentity is the exact agent-native identity used to join cmux hooks.
type SessionIdentity struct {
	Agent           string
	NativeSessionID string
	SessionID       string
}

// ReplaceCmuxSnapshot atomically marks old observations closed and stores the
// latest successful or failed cmux observation without erasing push provenance.
func (s *Store) ReplaceCmuxSnapshot(ctx context.Context, status CmuxStatus, states []CmuxSessionState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cmux snapshot replacement: %w", err)
	}
	defer tx.Rollback()

	if status.ObservedAt.IsZero() {
		status.ObservedAt = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO cmux_status(id, available, access_mode, error, observed_at)
        VALUES (1, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            available = excluded.available,
            access_mode = excluded.access_mode,
            error = excluded.error,
            observed_at = excluded.observed_at`,
		status.Available, status.AccessMode, status.Error, formatTime(status.ObservedAt)); err != nil {
		return fmt.Errorf("replace cmux status: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cmux_session_state SET open = 0`); err != nil {
		return fmt.Errorf("close stale cmux sessions: %w", err)
	}
	for _, state := range states {
		if state.ObservedAt.IsZero() {
			state.ObservedAt = status.ObservedAt
		}
		if state.Lifecycle == "" {
			state.Lifecycle = "unknown"
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO cmux_session_state(
                session_id, open, workspace_id, surface_id, workspace_title,
                surface_title, workspace_has_custom_title, lifecycle, observed_at
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(session_id) DO UPDATE SET
                open = excluded.open,
                workspace_id = excluded.workspace_id,
                surface_id = excluded.surface_id,
                workspace_title = excluded.workspace_title,
                surface_title = excluded.surface_title,
                workspace_has_custom_title = excluded.workspace_has_custom_title,
                lifecycle = excluded.lifecycle,
                observed_at = excluded.observed_at`,
			state.SessionID, state.Open, state.WorkspaceID, state.SurfaceID,
			state.WorkspaceTitle, state.SurfaceTitle, state.WorkspaceHasCustomTitle,
			state.Lifecycle, formatTime(state.ObservedAt)); err != nil {
			return fmt.Errorf("replace cmux session %s: %w", state.SessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cmux snapshot replacement: %w", err)
	}
	return nil
}

// RecordCmuxPush records only nonempty targets that were successfully renamed.
func (s *Store) RecordCmuxPush(ctx context.Context, sessionID, workspaceTitle, surfaceTitle string, pushedAt time.Time) error {
	if pushedAt.IsZero() {
		pushedAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `
        UPDATE cmux_session_state SET
            last_pushed_workspace_title = coalesce(nullif(?, ''), last_pushed_workspace_title),
            last_pushed_surface_title = coalesce(nullif(?, ''), last_pushed_surface_title),
            last_pushed_at = ?
        WHERE session_id = ?`, workspaceTitle, surfaceTitle, formatTime(pushedAt), sessionID)
	if err != nil {
		return fmt.Errorf("record cmux title push: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read cmux title push result: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// CmuxState returns the last observed state and push provenance for a session.
func (s *Store) CmuxState(ctx context.Context, sessionID string) (CmuxSessionState, bool, error) {
	state, err := scanCmuxState(s.db.QueryRowContext(ctx, cmuxStateSelect+` WHERE session_id = ?`, sessionID))
	if err == sql.ErrNoRows {
		return CmuxSessionState{}, false, nil
	}
	if err != nil {
		return CmuxSessionState{}, false, fmt.Errorf("get cmux session state: %w", err)
	}
	return state, true, nil
}

// CmuxStatus returns the last API availability observation.
func (s *Store) CmuxStatus(ctx context.Context) (CmuxStatus, error) {
	var status CmuxStatus
	var observedAt string
	err := s.db.QueryRowContext(ctx, `
        SELECT available, access_mode, error, observed_at FROM cmux_status WHERE id = 1`).
		Scan(&status.Available, &status.AccessMode, &status.Error, &observedAt)
	if err == sql.ErrNoRows {
		return CmuxStatus{}, nil
	}
	if err != nil {
		return CmuxStatus{}, fmt.Errorf("get cmux status: %w", err)
	}
	status.ObservedAt, err = parseTime(observedAt)
	if err != nil {
		return CmuxStatus{}, fmt.Errorf("parse cmux status time: %w", err)
	}
	return status, nil
}

// SessionsByNativeIdentity lists the stable join keys without transcript data.
func (s *Store) SessionsByNativeIdentity(ctx context.Context) ([]SessionIdentity, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT agent, native_session_id, id
        FROM sessions ORDER BY agent, native_session_id, id`)
	if err != nil {
		return nil, fmt.Errorf("list session identities: %w", err)
	}
	defer rows.Close()
	var result []SessionIdentity
	for rows.Next() {
		var identity SessionIdentity
		if err := rows.Scan(&identity.Agent, &identity.NativeSessionID, &identity.SessionID); err != nil {
			return nil, fmt.Errorf("scan session identity: %w", err)
		}
		result = append(result, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list session identities: %w", err)
	}
	return result, nil
}

const cmuxStateSelect = `SELECT
    session_id, open, workspace_id, surface_id, workspace_title, surface_title,
    workspace_has_custom_title, lifecycle, observed_at,
    last_pushed_workspace_title, last_pushed_surface_title, last_pushed_at
FROM cmux_session_state`

func scanCmuxState(row rowScanner) (CmuxSessionState, error) {
	var state CmuxSessionState
	var observedAt string
	var pushedWorkspace, pushedSurface, pushedAt sql.NullString
	if err := row.Scan(
		&state.SessionID, &state.Open, &state.WorkspaceID, &state.SurfaceID,
		&state.WorkspaceTitle, &state.SurfaceTitle, &state.WorkspaceHasCustomTitle,
		&state.Lifecycle, &observedAt, &pushedWorkspace, &pushedSurface, &pushedAt,
	); err != nil {
		return CmuxSessionState{}, err
	}
	var err error
	state.ObservedAt, err = parseTime(observedAt)
	if err != nil {
		return CmuxSessionState{}, err
	}
	state.LastPushedWorkspaceTitle = pushedWorkspace.String
	state.LastPushedSurfaceTitle = pushedSurface.String
	if pushedAt.Valid {
		state.LastPushedAt, err = parseTime(pushedAt.String)
		if err != nil {
			return CmuxSessionState{}, err
		}
	}
	return state, nil
}

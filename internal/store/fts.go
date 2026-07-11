package store

import (
	"context"
	"database/sql"
	"fmt"
)

// RebuildFTS deterministically regenerates all search documents from the
// ordinary tables.
func (s *Store) RebuildFTS(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin FTS rebuild: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_fts`); err != nil {
		return fmt.Errorf("clear FTS: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list sessions for FTS rebuild: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan session for FTS rebuild: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close FTS session rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list sessions for FTS rebuild: %w", err)
	}
	for _, id := range ids {
		if err := rebuildSessionFTS(ctx, tx, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit FTS rebuild: %w", err)
	}
	return nil
}

func rebuildSessionFTS(ctx context.Context, tx *sql.Tx, sessionID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_fts WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("clear session FTS: %w", err)
	}
	var title, summary sql.NullString
	var cwd string
	if err := tx.QueryRowContext(ctx,
		`SELECT title, summary, working_directory FROM sessions WHERE id = ?`, sessionID,
	).Scan(&title, &summary, &cwd); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("read session for FTS: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO session_fts(session_id, document_key, document_type, title, body, working_directory)
        VALUES (?, ?, 'session', ?, ?, ?)`, sessionID, "session", title.String, summary.String, cwd); err != nil {
		return fmt.Errorf("index session: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
        SELECT sequence, role, text, coalesce(tool_name, '')
        FROM messages WHERE session_id = ? ORDER BY sequence`, sessionID)
	if err != nil {
		return fmt.Errorf("read messages for FTS: %w", err)
	}
	for rows.Next() {
		var sequence int
		var role, text, toolName string
		if err := rows.Scan(&sequence, &role, &text, &toolName); err != nil {
			rows.Close()
			return fmt.Errorf("scan message for FTS: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO session_fts(session_id, document_key, document_type, title, body, working_directory)
            VALUES (?, printf('message:%d', ?), ?, ?, ?, ?)`,
			sessionID, sequence, "message", toolName, text, cwd); err != nil {
			rows.Close()
			return fmt.Errorf("index message %d: %w", sequence, err)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close message FTS rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read messages for FTS: %w", err)
	}

	rows, err = tx.QueryContext(ctx, `
        SELECT position, title, summary, detail
        FROM segments WHERE session_id = ? ORDER BY position`, sessionID)
	if err != nil {
		return fmt.Errorf("read segments for FTS: %w", err)
	}
	for rows.Next() {
		var position int
		var segmentTitle, segmentSummary, detail string
		if err := rows.Scan(&position, &segmentTitle, &segmentSummary, &detail); err != nil {
			rows.Close()
			return fmt.Errorf("scan segment for FTS: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO session_fts(session_id, document_key, document_type, title, body, working_directory)
            VALUES (?, printf('segment:%d', ?), 'segment', ?, ?, ?)`,
			sessionID, position, segmentTitle, segmentSummary+"\n"+detail, cwd); err != nil {
			rows.Close()
			return fmt.Errorf("index segment %d: %w", position, err)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close segment FTS rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read segments for FTS: %w", err)
	}
	return nil
}

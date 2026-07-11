package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SessionSourceState returns the persisted discovery identity for a native
// provider session.
func (s *Store) SessionSourceState(ctx context.Context, agent, nativeSessionID string) (SourceState, bool, error) {
	var state SourceState
	var sourceTime string
	err := s.db.QueryRowContext(ctx, `
        SELECT id, source_path, source_size, source_mtime, source_hash
        FROM sessions WHERE agent = ? AND native_session_id = ?`,
		agent, nativeSessionID,
	).Scan(&state.SessionID, &state.SourcePath, &state.SourceSize, &sourceTime, &state.SourceHash)
	if err == sql.ErrNoRows {
		return SourceState{}, false, nil
	}
	if err != nil {
		return SourceState{}, false, fmt.Errorf("read source state: %w", err)
	}
	state.SourceTime, err = parseTime(sourceTime)
	if err != nil {
		return SourceState{}, false, fmt.Errorf("parse source mtime: %w", err)
	}
	return state, true, nil
}

// ImportSession atomically refreshes source metadata, normalized messages, FTS
// documents, and summary nodes invalidated by a transcript rewrite.
func (s *Store) ImportSession(ctx context.Context, session Session, messages []Message) (ImportResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ImportResult{}, fmt.Errorf("begin session import: %w", err)
	}
	defer tx.Rollback()

	previous, err := messagesInTx(ctx, tx, session.ID)
	if err != nil {
		return ImportResult{}, err
	}
	firstChanged := unchangedPrefix(previous, messages)
	changed := firstChanged != len(previous) || firstChanged != len(messages)
	if err := upsertSession(ctx, tx, session); err != nil {
		return ImportResult{}, err
	}
	if changed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, session.ID); err != nil {
			return ImportResult{}, fmt.Errorf("delete imported messages: %w", err)
		}
		for _, message := range messages {
			if _, err := tx.ExecContext(ctx, `
                INSERT INTO messages(session_id, sequence, timestamp, role, text, tool_name)
                VALUES (?, ?, ?, ?, ?, ?)`, session.ID, message.Sequence,
				formatTime(message.Timestamp), message.Role, message.Text,
				nullableText(message.ToolName)); err != nil {
				return ImportResult{}, fmt.Errorf("insert imported message %d: %w", message.Sequence, err)
			}
		}
		if err := invalidateSummarySuffix(ctx, tx, session.ID, firstChanged); err != nil {
			return ImportResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `
            UPDATE sessions
            SET analysis_status = CASE WHEN analysis_status = 'current' THEN 'partial' ELSE analysis_status END,
                updated_at = ?
            WHERE id = ?`, formatTime(time.Now()), session.ID); err != nil {
			return ImportResult{}, fmt.Errorf("mark changed analysis partial: %w", err)
		}
	}
	if err := rebuildSessionFTS(ctx, tx, session.ID); err != nil {
		return ImportResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImportResult{}, fmt.Errorf("commit session import: %w", err)
	}
	return ImportResult{Changed: changed, FirstChangedSequence: firstChanged}, nil
}

func messagesInTx(ctx context.Context, tx *sql.Tx, sessionID string) ([]Message, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT session_id, sequence, timestamp, role, text, coalesce(tool_name, '')
        FROM messages WHERE session_id = ? ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read existing messages: %w", err)
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var message Message
		var timestamp string
		if err := rows.Scan(&message.SessionID, &message.Sequence, &timestamp,
			&message.Role, &message.Text, &message.ToolName); err != nil {
			return nil, fmt.Errorf("scan existing message: %w", err)
		}
		message.Timestamp, err = parseTime(timestamp)
		if err != nil {
			return nil, fmt.Errorf("parse existing message time: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read existing messages: %w", err)
	}
	return messages, nil
}

func unchangedPrefix(previous, current []Message) int {
	limit := min(len(previous), len(current))
	for i := 0; i < limit; i++ {
		if previous[i].Sequence != current[i].Sequence ||
			!previous[i].Timestamp.Equal(current[i].Timestamp) ||
			previous[i].Role != current[i].Role ||
			previous[i].Text != current[i].Text ||
			previous[i].ToolName != current[i].ToolName {
			return i
		}
	}
	return limit
}

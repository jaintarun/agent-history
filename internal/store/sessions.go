package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// UpsertSession inserts source metadata or refreshes the existing record.
func (s *Store) UpsertSession(ctx context.Context, session Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session upsert: %w", err)
	}
	defer tx.Rollback()
	if err := upsertSession(ctx, tx, session); err != nil {
		return err
	}
	if err := rebuildSessionFTS(ctx, tx, session.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session upsert: %w", err)
	}
	return nil
}

func upsertSession(ctx context.Context, tx *sql.Tx, session Session) error {
	now := time.Now().UTC()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = now
	}
	if session.AnalysisStatus == "" {
		session.AnalysisStatus = "none"
	}
	_, err := tx.ExecContext(ctx, `
        INSERT INTO sessions(
            id, agent, native_session_id, source_path, source_size, source_mtime,
            source_hash, working_directory, started_at, last_active_at,
            analysis_status, created_at, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            agent = excluded.agent,
            native_session_id = excluded.native_session_id,
            source_path = excluded.source_path,
            source_size = excluded.source_size,
            source_mtime = excluded.source_mtime,
            source_hash = excluded.source_hash,
            working_directory = excluded.working_directory,
            started_at = excluded.started_at,
            last_active_at = excluded.last_active_at,
            updated_at = excluded.updated_at`,
		session.ID, session.Agent, session.NativeSessionID, session.SourcePath,
		session.SourceSize, formatTime(session.SourceMTime), session.SourceHash,
		session.WorkingDirectory, formatTime(session.StartedAt),
		formatTime(session.LastActiveAt), session.AnalysisStatus,
		formatTime(session.CreatedAt), formatTime(session.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// ReplaceMessages atomically replaces all normalized messages for a session.
func (s *Store) ReplaceMessages(ctx context.Context, sessionID string, messages []Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin message replacement: %w", err)
	}
	defer tx.Rollback()
	if err := requireSession(ctx, tx, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}
	for _, message := range messages {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO messages(session_id, sequence, timestamp, role, text, tool_name)
            VALUES (?, ?, ?, ?, ?, ?)`, sessionID, message.Sequence,
			formatTime(message.Timestamp), message.Role, message.Text,
			nullableText(message.ToolName)); err != nil {
			return fmt.Errorf("insert message %d: %w", message.Sequence, err)
		}
	}
	if err := rebuildSessionFTS(ctx, tx, sessionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit message replacement: %w", err)
	}
	return nil
}

// GetSession returns source metadata, normalized messages, and visible topics.
func (s *Store) GetSession(ctx context.Context, id string) (SessionDetail, error) {
	session, err := scanSession(s.db.QueryRowContext(ctx, sessionSelect+` WHERE id = ?`, id))
	if err != nil {
		if err == sql.ErrNoRows {
			return SessionDetail{}, ErrNotFound
		}
		return SessionDetail{}, fmt.Errorf("get session: %w", err)
	}
	messages, err := s.messages(ctx, id)
	if err != nil {
		return SessionDetail{}, err
	}
	segments, err := s.segments(ctx, id)
	if err != nil {
		return SessionDetail{}, err
	}
	return SessionDetail{Session: session, Messages: messages, Segments: segments}, nil
}

// DeleteSession removes a session and all derived records.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session deletion: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_fts WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete session FTS: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count == 0 {
		if err != nil {
			return fmt.Errorf("session deletion result: %w", err)
		}
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session deletion: %w", err)
	}
	return nil
}

const sessionSelect = `SELECT
    id, agent, native_session_id, source_path, source_size, source_mtime,
    source_hash, working_directory, title, summary, topic_count,
    started_at, last_active_at, analysis_status, analysis_error,
    analysis_provider, analysis_model, analysis_prompt_version, analyzed_at,
    analyzed_hash, analyzed_through_sequence, analyzed_through_at, created_at, updated_at
FROM sessions`

type rowScanner interface {
	Scan(...any) error
}

func scanSession(row rowScanner) (Session, error) {
	var session Session
	var sourceMTime, startedAt, lastActiveAt, createdAt, updatedAt string
	var title, summary, analysisError, provider, model, promptVersion sql.NullString
	var analyzedAt, analyzedHash, analyzedThroughAt sql.NullString
	var topicCount, analyzedThroughSequence sql.NullInt64
	err := row.Scan(
		&session.ID, &session.Agent, &session.NativeSessionID, &session.SourcePath,
		&session.SourceSize, &sourceMTime, &session.SourceHash, &session.WorkingDirectory,
		&title, &summary, &topicCount, &startedAt, &lastActiveAt,
		&session.AnalysisStatus, &analysisError, &provider, &model, &promptVersion,
		&analyzedAt, &analyzedHash, &analyzedThroughSequence, &analyzedThroughAt,
		&createdAt, &updatedAt)
	if err != nil {
		return Session{}, err
	}
	session.SourceMTime, err = parseTime(sourceMTime)
	if err != nil {
		return Session{}, err
	}
	session.StartedAt, err = parseTime(startedAt)
	if err != nil {
		return Session{}, err
	}
	session.LastActiveAt, err = parseTime(lastActiveAt)
	if err != nil {
		return Session{}, err
	}
	session.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return Session{}, err
	}
	session.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return Session{}, err
	}
	session.Title, session.Summary = title.String, summary.String
	session.TopicCount = int(topicCount.Int64)
	session.AnalysisError = analysisError.String
	session.AnalysisProvider, session.AnalysisModel = provider.String, model.String
	session.AnalysisPromptVersion, session.AnalyzedHash = promptVersion.String, analyzedHash.String
	if analyzedThroughSequence.Valid {
		value := int(analyzedThroughSequence.Int64)
		session.AnalyzedThroughSequence = &value
	}
	session.AnalyzedAt, err = parseTime(analyzedAt.String)
	if err != nil {
		return Session{}, err
	}
	session.AnalyzedThroughAt, err = parseTime(analyzedThroughAt.String)
	return session, err
}

func (s *Store) messages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT session_id, sequence, timestamp, role, text, tool_name
        FROM messages WHERE session_id = ? ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var message Message
		var timestamp string
		var toolName sql.NullString
		if err := rows.Scan(&message.SessionID, &message.Sequence, &timestamp, &message.Role, &message.Text, &toolName); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		message.Timestamp, err = parseTime(timestamp)
		if err != nil {
			return nil, fmt.Errorf("parse message timestamp: %w", err)
		}
		message.ToolName = toolName.String
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) segments(ctx context.Context, sessionID string) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT session_id, position, start_sequence, end_sequence, title, summary, detail
        FROM segments WHERE session_id = ? ORDER BY position`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query segments: %w", err)
	}
	defer rows.Close()
	var segments []Segment
	for rows.Next() {
		var segment Segment
		if err := rows.Scan(&segment.SessionID, &segment.Position, &segment.StartSequence,
			&segment.EndSequence, &segment.Title, &segment.Summary, &segment.Detail); err != nil {
			return nil, fmt.Errorf("scan segment: %w", err)
		}
		segments = append(segments, segment)
	}
	return segments, rows.Err()
}

func requireSession(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("check session: %w", err)
	}
	return nil
}

// SetSetting writes a non-secret setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	lower := strings.ToLower(key)
	for _, prohibited := range []string{"api_key", "apikey", "token", "secret", "password", "credential"} {
		if strings.Contains(lower, prohibited) {
			return fmt.Errorf("setting %q may contain credentials", key)
		}
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("setting key is empty")
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO settings(key, value, updated_at) VALUES (?, ?, ?)
        ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("set setting: %w", err)
	}
	return nil
}

// Setting reads a setting.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get setting: %w", err)
	}
	return value, true, nil
}

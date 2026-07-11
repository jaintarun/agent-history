package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ReplaceAnalysis atomically replaces visible analysis and its summary tree.
func (s *Store) ReplaceAnalysis(ctx context.Context, sessionID string, analysis Analysis) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin analysis replacement: %w", err)
	}
	defer tx.Rollback()
	if err := requireSession(ctx, tx, sessionID); err != nil {
		return err
	}
	if analysis.Status == "" {
		analysis.Status = "current"
	}
	if analysis.AnalyzedAt.IsZero() {
		analysis.AnalyzedAt = time.Now().UTC()
	}
	_, err = tx.ExecContext(ctx, `
        UPDATE sessions SET
            title = ?, summary = ?, topic_count = ?, analysis_status = ?,
            analysis_error = ?, analysis_provider = ?, analysis_model = ?,
            analysis_prompt_version = ?, analyzed_at = ?, analyzed_hash = ?,
            analyzed_through_sequence = ?, analyzed_through_at = ?, updated_at = ?
        WHERE id = ?`,
		nullableText(analysis.Title), nullableText(analysis.Summary), len(analysis.Segments),
		analysis.Status, nullableText(analysis.Error), nullableText(analysis.Provider),
		nullableText(analysis.Model), nullableText(analysis.PromptVersion),
		nullableTime(analysis.AnalyzedAt), nullableText(analysis.AnalyzedHash),
		nullableInt(analysis.AnalyzedThroughSequence), nullableTime(analysis.AnalyzedThroughAt),
		formatTime(time.Now()), sessionID)
	if err != nil {
		return fmt.Errorf("update analysis metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM segments WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete segments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM summary_nodes WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete summary nodes: %w", err)
	}
	for _, segment := range analysis.Segments {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO segments(session_id, position, start_sequence, end_sequence, title, summary, detail)
            VALUES (?, ?, ?, ?, ?, ?, ?)`, sessionID, segment.Position,
			segment.StartSequence, segment.EndSequence, segment.Title, segment.Summary,
			segment.Detail); err != nil {
			return fmt.Errorf("insert segment %d: %w", segment.Position, err)
		}
	}
	for _, node := range analysis.Nodes {
		node.SessionID = sessionID
		if err := putSummaryNode(ctx, tx, node); err != nil {
			return err
		}
	}
	if err := rebuildSessionFTS(ctx, tx, sessionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit analysis replacement: %w", err)
	}
	return nil
}

// DeleteAnalysis removes generated data but preserves source metadata and
// normalized messages.
func (s *Store) DeleteAnalysis(ctx context.Context, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin analysis deletion: %w", err)
	}
	defer tx.Rollback()
	if err := requireSession(ctx, tx, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
        UPDATE sessions SET
            title = NULL, summary = NULL, topic_count = NULL,
            analysis_status = 'none', analysis_error = NULL,
            analysis_provider = NULL, analysis_model = NULL,
            analysis_prompt_version = NULL, analyzed_at = NULL,
            analyzed_hash = NULL, analyzed_through_sequence = NULL,
            analyzed_through_at = NULL, updated_at = ?
        WHERE id = ?`, formatTime(time.Now()), sessionID); err != nil {
		return fmt.Errorf("clear analysis metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM segments WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete segments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM summary_nodes WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete summary nodes: %w", err)
	}
	if err := rebuildSessionFTS(ctx, tx, sessionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit analysis deletion: %w", err)
	}
	return nil
}

// SetAnalysisStatus updates worker state without replacing visible analysis.
func (s *Store) SetAnalysisStatus(ctx context.Context, sessionID, status, message string) error {
	result, err := s.db.ExecContext(ctx, `
        UPDATE sessions SET analysis_status = ?, analysis_error = ?, updated_at = ?
        WHERE id = ?`, status, nullableText(message), formatTime(time.Now()), sessionID)
	if err != nil {
		return fmt.Errorf("set analysis status: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("analysis status result: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// PendingAnalysisSessionIDs returns nonempty sessions that have not been
// analyzed or have new imported activity. includeFailed supports one bounded
// retry at process startup without retrying failures after every periodic scan.
func (s *Store) PendingAnalysisSessionIDs(ctx context.Context, includeFailed bool) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT sessions.id
        FROM sessions
        WHERE (
              sessions.analysis_status IN ('none', 'partial')
              OR (? AND sessions.analysis_status = 'failed')
          )
          AND EXISTS (
              SELECT 1 FROM messages WHERE messages.session_id = sessions.id
          )
        ORDER BY sessions.last_active_at DESC, sessions.id`, includeFailed)
	if err != nil {
		return nil, fmt.Errorf("list pending analysis sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pending analysis session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending analysis sessions: %w", err)
	}
	return ids, nil
}

// RecoverAnalysisStates resets jobs left queued or running by an unclean exit.
// It returns the affected session IDs so an auto-analysis caller can requeue
// them after the transaction commits.
func (s *Store) RecoverAnalysisStates(ctx context.Context, auto bool) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin analysis recovery: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
        SELECT id FROM sessions
        WHERE analysis_status IN ('queued', 'running')
        ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list stale analysis states: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan stale analysis state: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stale analysis states: %w", err)
	}
	statusExpression := `CASE
            WHEN analysis_provider IS NULL THEN 'none'
            WHEN analyzed_through_sequence < COALESCE(
                (SELECT MAX(sequence) FROM messages WHERE session_id = sessions.id),
                analyzed_through_sequence
            ) THEN 'partial'
            ELSE 'current'
        END`
	if auto {
		statusExpression = `'queued'`
	}
	query := `UPDATE sessions SET analysis_status = ` + statusExpression + `,
        analysis_error = NULL, updated_at = ?
        WHERE analysis_status IN ('queued', 'running')`
	if _, err := tx.ExecContext(ctx, query, formatTime(time.Now())); err != nil {
		return nil, fmt.Errorf("recover analysis states: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit analysis recovery: %w", err)
	}
	return ids, nil
}

// PutSummaryNode inserts or refreshes one content-addressed summary node.
func (s *Store) PutSummaryNode(ctx context.Context, node SummaryNode) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin summary node write: %w", err)
	}
	defer tx.Rollback()
	if err := putSummaryNode(ctx, tx, node); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit summary node write: %w", err)
	}
	return nil
}

func putSummaryNode(ctx context.Context, tx *sql.Tx, node SummaryNode) error {
	now := time.Now().UTC()
	if node.CreatedAt.IsZero() {
		node.CreatedAt = now
	}
	if node.UpdatedAt.IsZero() {
		node.UpdatedAt = now
	}
	_, err := tx.ExecContext(ctx, `
        INSERT INTO summary_nodes(
            id, session_id, parent_id, kind, position, start_sequence,
            end_sequence, input_hash, summary_json, sealed, provider, model,
            prompt_version, normalizer_version, created_at, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            parent_id = excluded.parent_id,
            position = excluded.position,
            summary_json = excluded.summary_json,
            sealed = excluded.sealed,
            updated_at = excluded.updated_at`,
		node.ID, node.SessionID, node.ParentID, node.Kind, node.Position,
		node.StartSequence, node.EndSequence, node.InputHash, node.SummaryJSON,
		node.Sealed, node.Provider, node.Model, node.PromptVersion,
		node.NormalizerVersion, formatTime(node.CreatedAt), formatTime(node.UpdatedAt))
	if err != nil {
		return fmt.Errorf("put summary node %q: %w", node.ID, err)
	}
	return nil
}

// FindSummaryNode finds a node using its complete cache identity.
func (s *Store) FindSummaryNode(ctx context.Context, key NodeCacheKey) (SummaryNode, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, session_id, parent_id, kind, position, start_sequence,
               end_sequence, input_hash, summary_json, sealed, provider, model,
               prompt_version, normalizer_version, created_at, updated_at
        FROM summary_nodes
        WHERE session_id = ? AND kind = ? AND start_sequence = ? AND end_sequence = ?
          AND input_hash = ? AND provider = ? AND model = ?
          AND prompt_version = ? AND normalizer_version = ?`,
		key.SessionID, key.Kind, key.StartSequence, key.EndSequence, key.InputHash,
		key.Provider, key.Model, key.PromptVersion, key.NormalizerVersion)
	return scanSummaryNode(row)
}

// SummaryNodes returns cached nodes for one complete analysis provenance.
func (s *Store) SummaryNodes(ctx context.Context, sessionID, kind, provider, model, promptVersion, normalizerVersion string) ([]SummaryNode, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, session_id, parent_id, kind, position, start_sequence,
               end_sequence, input_hash, summary_json, sealed, provider, model,
               prompt_version, normalizer_version, created_at, updated_at
        FROM summary_nodes
        WHERE session_id = ? AND kind = ? AND provider = ? AND model = ?
          AND prompt_version = ? AND normalizer_version = ?
        ORDER BY start_sequence, end_sequence, position`,
		sessionID, kind, provider, model, promptVersion, normalizerVersion)
	if err != nil {
		return nil, fmt.Errorf("list summary nodes: %w", err)
	}
	defer rows.Close()
	var nodes []SummaryNode
	for rows.Next() {
		node, err := scanSummaryNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list summary nodes: %w", err)
	}
	return nodes, nil
}

// DeleteSummaryNode removes a node and descendants linked through parent_id.
func (s *Store) DeleteSummaryNode(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM summary_nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete summary node: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("summary node deletion result: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// InvalidateSummarySuffix deletes nodes overlapping a changed suffix and every
// ancestor that depends on them. Unchanged siblings are detached before an
// invalid ancestor is removed so their content-addressed results remain reusable.
func (s *Store) InvalidateSummarySuffix(ctx context.Context, sessionID string, firstChangedSequence int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin summary invalidation: %w", err)
	}
	defer tx.Rollback()
	if err := invalidateSummarySuffix(ctx, tx, sessionID, firstChangedSequence); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit summary invalidation: %w", err)
	}
	return nil
}

func invalidateSummarySuffix(ctx context.Context, tx *sql.Tx, sessionID string, firstChangedSequence int) error {
	rows, err := tx.QueryContext(ctx, `
        WITH RECURSIVE invalid(id, parent_id) AS (
            SELECT id, parent_id
            FROM summary_nodes
            WHERE session_id = ? AND end_sequence >= ?
            UNION
            SELECT parent.id, parent.parent_id
            FROM summary_nodes AS parent
            JOIN invalid AS child ON parent.id = child.parent_id
        )
        SELECT id FROM invalid`, sessionID, firstChangedSequence)
	if err != nil {
		return fmt.Errorf("find invalid summary nodes: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan invalid summary node: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close invalid summary rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("find invalid summary nodes: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)*2)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE summary_nodes SET parent_id = NULL WHERE parent_id IN (`+placeholders+`) AND id NOT IN (`+placeholders+`)`,
		args...); err != nil {
		return fmt.Errorf("detach reusable summary nodes: %w", err)
	}
	deleteArgs := make([]any, len(ids))
	for i, id := range ids {
		deleteArgs[i] = id
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM summary_nodes WHERE id IN (`+placeholders+`)`, deleteArgs...); err != nil {
		return fmt.Errorf("delete invalid summary nodes: %w", err)
	}
	return nil
}

func scanSummaryNode(row rowScanner) (SummaryNode, error) {
	var node SummaryNode
	var parentID sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(&node.ID, &node.SessionID, &parentID, &node.Kind, &node.Position,
		&node.StartSequence, &node.EndSequence, &node.InputHash, &node.SummaryJSON,
		&node.Sealed, &node.Provider, &node.Model, &node.PromptVersion,
		&node.NormalizerVersion, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return SummaryNode{}, ErrNotFound
	}
	if err != nil {
		return SummaryNode{}, fmt.Errorf("scan summary node: %w", err)
	}
	if parentID.Valid {
		node.ParentID = &parentID.String
	}
	node.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return SummaryNode{}, fmt.Errorf("parse summary node created time: %w", err)
	}
	node.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return SummaryNode{}, fmt.Errorf("parse summary node updated time: %w", err)
	}
	return node, nil
}

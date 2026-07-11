package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOpenMigratesEmptyDatabaseAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")

	first, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	second, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	var migrations int
	if err := second.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&migrations); err != nil {
		t.Fatalf("query migrations: %v", err)
	}
	if migrations != 1 {
		t.Fatalf("migration count = %d, want 1", migrations)
	}

	var connections []*sql.Conn
	for i := 0; i < 4; i++ {
		conn, err := second.db.Conn(context.Background())
		if err != nil {
			t.Fatalf("acquire connection %d: %v", i, err)
		}
		connections = append(connections, conn)
		var foreignKeys int
		if err := conn.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatalf("query foreign keys on connection %d: %v", i, err)
		}
		if foreignKeys != 1 {
			t.Fatalf("foreign keys on connection %d = %d, want 1", i, foreignKeys)
		}
	}
	for _, conn := range connections {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReplaceMessagesIsAtomicAndForeignKeysCascade(t *testing.T) {
	store := openTestStore(t)
	session := testSession("session-1")
	if err := store.UpsertSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}

	messages := []Message{
		{Sequence: 1, Timestamp: session.StartedAt, Role: "user", Text: "find the token refresh bug"},
		{Sequence: 2, Timestamp: session.LastActiveAt, Role: "assistant", Text: "the cache key omitted the tenant"},
	}
	if err := store.ReplaceMessages(context.Background(), session.ID, messages); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMessages(context.Background(), session.ID, messages[:1]); err != nil {
		t.Fatal(err)
	}

	detail, err := store.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 1 || detail.Messages[0].Text != messages[0].Text {
		t.Fatalf("messages = %#v, want first replacement message only", detail.Messages)
	}
	if detail.Session.StartedAt.Location() != time.UTC || detail.Messages[0].Timestamp.Location() != time.UTC {
		t.Fatalf("stored times are not UTC: session=%v message=%v", detail.Session.StartedAt, detail.Messages[0].Timestamp)
	}

	if err := store.DeleteSession(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM messages WHERE session_id = ?`, session.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("message count after session deletion = %d, want 0", count)
	}
}

func TestReplaceAnalysisIsAtomicAndDeletePreservesMessages(t *testing.T) {
	store := openTestStore(t)
	session := testSession("session-analysis")
	if err := store.UpsertSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMessages(context.Background(), session.ID, []Message{{
		Sequence: 1, Timestamp: session.StartedAt, Role: "user", Text: "design the index",
	}}); err != nil {
		t.Fatal(err)
	}

	first := Analysis{
		Title: "Search index design", Summary: "Designed the local search index.", Status: "current",
		Provider: "fake", Model: "test", PromptVersion: "v1", AnalyzedAt: session.LastActiveAt,
		AnalyzedHash: "hash-1", AnalyzedThroughSequence: intPointer(1), AnalyzedThroughAt: session.LastActiveAt,
		Segments: []Segment{{Position: 0, StartSequence: 1, EndSequence: 1, Title: "Index", Summary: "Designed indexing.", Detail: "Compared FTS options."}},
	}
	if err := store.ReplaceAnalysis(context.Background(), session.ID, first); err != nil {
		t.Fatal(err)
	}

	invalid := first
	invalid.Title = "must not replace"
	invalid.Segments = append(invalid.Segments, invalid.Segments[0])
	if err := store.ReplaceAnalysis(context.Background(), session.ID, invalid); err == nil {
		t.Fatal("ReplaceAnalysis with duplicate segment position succeeded, want error")
	}

	detail, err := store.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.Title != first.Title || len(detail.Segments) != 1 {
		t.Fatalf("analysis changed after failed replacement: %#v", detail)
	}

	if err := store.DeleteAnalysis(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	detail, err = store.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.Title != "" || len(detail.Segments) != 0 || len(detail.Messages) != 1 {
		t.Fatalf("detail after DeleteAnalysis = %#v", detail)
	}
}

func TestFTSUpdatesDeletesAndRebuilds(t *testing.T) {
	store := openTestStore(t)
	session := testSession("session-fts")
	if err := store.UpsertSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMessages(context.Background(), session.ID, []Message{{
		Sequence: 1, Timestamp: session.StartedAt, Role: "user", Text: "unusual refresh failure",
	}}); err != nil {
		t.Fatal(err)
	}
	assertFTSCount(t, store, "refresh", 1)

	if err := store.ReplaceMessages(context.Background(), session.ID, []Message{{
		Sequence: 1, Timestamp: session.StartedAt, Role: "user", Text: "different problem",
	}}); err != nil {
		t.Fatal(err)
	}
	assertFTSCount(t, store, "refresh", 0)
	assertFTSCount(t, store, "different", 1)

	if _, err := store.db.Exec(`DELETE FROM session_fts`); err != nil {
		t.Fatal(err)
	}
	assertFTSCount(t, store, "different", 0)
	if err := store.RebuildFTS(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertFTSCount(t, store, "different", 1)
}

func TestSummaryNodeCacheIdentityAndCascade(t *testing.T) {
	store := openTestStore(t)
	session := testSession("session-nodes")
	if err := store.UpsertSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	parent := testNode("parent", session.ID, nil, "topic", 0, 1, 4)
	childParent := parent.ID
	child := testNode("child", session.ID, &childParent, "leaf", 0, 1, 2)
	if err := store.PutSummaryNode(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSummaryNode(context.Background(), child); err != nil {
		t.Fatal(err)
	}

	found, err := store.FindSummaryNode(context.Background(), NodeCacheKey{
		SessionID: session.ID, Kind: child.Kind, StartSequence: child.StartSequence,
		EndSequence: child.EndSequence, InputHash: child.InputHash, Provider: child.Provider,
		Model: child.Model, PromptVersion: child.PromptVersion, NormalizerVersion: child.NormalizerVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != child.ID {
		t.Fatalf("found node ID = %q, want %q", found.ID, child.ID)
	}

	if err := store.DeleteSummaryNode(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM summary_nodes WHERE session_id = ?`, session.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("node count after parent deletion = %d, want 0", count)
	}
}

func TestInvalidateSummarySuffixPreservesUnaffectedSibling(t *testing.T) {
	store := openTestStore(t)
	session := testSession("session-invalidation")
	if err := store.UpsertSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	parent := testNode("rollup", session.ID, nil, "topic", 0, 1, 4)
	parentID := parent.ID
	unchanged := testNode("unchanged", session.ID, &parentID, "leaf", 0, 1, 2)
	affected := testNode("affected", session.ID, &parentID, "leaf", 1, 3, 4)
	for _, node := range []SummaryNode{parent, unchanged, affected} {
		if err := store.PutSummaryNode(context.Background(), node); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.InvalidateSummarySuffix(context.Background(), session.ID, 3); err != nil {
		t.Fatal(err)
	}
	var parentIDAfter sql.NullString
	if err := store.db.QueryRow(`SELECT parent_id FROM summary_nodes WHERE id = ?`, unchanged.ID).Scan(&parentIDAfter); err != nil {
		t.Fatalf("unchanged node was removed: %v", err)
	}
	if parentIDAfter.Valid {
		t.Fatalf("unchanged node parent = %q, want detached invalid ancestor", parentIDAfter.String)
	}
	var removed int
	if err := store.db.QueryRow(`SELECT count(*) FROM summary_nodes WHERE id IN (?, ?)`, parent.ID, affected.ID).Scan(&removed); err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("invalid node count = %d, want 0", removed)
	}
}

func TestSettingsRejectSecrets(t *testing.T) {
	store := openTestStore(t)
	if err := store.SetSetting(context.Background(), "analysis.model", "test-model"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Setting(context.Background(), "analysis.model")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "test-model" {
		t.Fatalf("Setting = %q, %v, want test-model, true", got, ok)
	}
	for _, key := range []string{"api_key", "analysis.token", "client_secret", "password"} {
		if err := store.SetSetting(context.Background(), key, "sensitive"); err == nil {
			t.Errorf("SetSetting(%q) succeeded, want rejection", key)
		}
	}
}

func TestConcurrentReaderAndSingleWriter(t *testing.T) {
	store := openTestStore(t)
	session := testSession("session-concurrent")
	if err := store.UpsertSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var readers sync.WaitGroup
	errCh := make(chan error, 2)
	for range 2 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for ctx.Err() == nil {
				if _, err := store.GetSession(ctx, session.ID); err != nil && ctx.Err() == nil {
					errCh <- err
					return
				}
			}
		}()
	}
	for i := 1; i <= 25; i++ {
		message := Message{Sequence: 1, Timestamp: session.StartedAt, Role: "user", Text: fmt.Sprintf("write %d", i)}
		if err := store.ReplaceMessages(context.Background(), session.ID, []Message{message}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	cancel()
	readers.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent read: %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func intPointer(value int) *int {
	return &value
}

func testSession(id string) Session {
	started := time.Date(2026, 7, 1, 10, 0, 0, 0, time.FixedZone("fixture", -5*60*60))
	return Session{
		ID: id, Agent: "codex", NativeSessionID: "native-" + id,
		SourcePath: "/tmp/" + id + ".jsonl", SourceSize: 100, SourceMTime: started,
		SourceHash: "source-" + id, WorkingDirectory: "/tmp/project",
		StartedAt: started, LastActiveAt: started.Add(2 * time.Hour), AnalysisStatus: "none",
	}
}

func testNode(id, sessionID string, parentID *string, kind string, position, start, end int) SummaryNode {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	return SummaryNode{
		ID: id, SessionID: sessionID, ParentID: parentID, Kind: kind, Position: position,
		StartSequence: start, EndSequence: end, InputHash: "input-" + id,
		SummaryJSON: `{"summary":"test"}`, Sealed: true, Provider: "fake", Model: "test",
		PromptVersion: "v1", NormalizerVersion: "v1", CreatedAt: now, UpdatedAt: now,
	}
}

func assertFTSCount(t *testing.T, store *Store, query string, want int) {
	t.Helper()
	var got int
	if err := store.db.QueryRow(`SELECT count(*) FROM session_fts WHERE session_fts MATCH ?`, query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("FTS count for %q = %d, want %d", query, got, want)
	}
}

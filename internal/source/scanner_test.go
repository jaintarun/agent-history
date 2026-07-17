package source_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaintarun/agent-history/internal/source"
	"github.com/jaintarun/agent-history/internal/source/claude"
	"github.com/jaintarun/agent-history/internal/source/codex"
	"github.com/jaintarun/agent-history/internal/store"

	_ "modernc.org/sqlite"
)

func TestCodexScanIsIdempotentAndInvalidatesOnlyChangedSuffix(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	transcript := filepath.Join(home, "sessions", "2026", "07", "rollout.jsonl")
	fixture, err := os.ReadFile(filepath.Join("codex", "testdata", "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, fixture, 0o600); err != nil {
		t.Fatal(err)
	}

	databasePath := filepath.Join(t.TempDir(), "history.db")
	database, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	scanner := source.NewScanner(database, codex.New(home))

	first, err := scanner.Scan(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if first.Discovered != 1 || first.Imported != 1 {
		t.Fatalf("first report = %#v", first)
	}
	sessionID := source.StableID("codex", "11111111-1111-4111-8111-111111111111")
	detail, err := database.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 5 {
		t.Fatalf("stored messages = %d, want 5", len(detail.Messages))
	}
	assertNoFTSMatch(t, databasePath, "private")
	assertNoFTSMatch(t, databasePath, "developer")

	second, err := scanner.Scan(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if second.Skipped != 1 || second.Imported != 0 {
		t.Fatalf("unchanged report = %#v", second)
	}

	node := store.SummaryNode{
		ID: "sealed-prefix", SessionID: sessionID, Kind: "leaf", Position: 0,
		StartSequence: 0, EndSequence: 4, InputHash: "prefix-hash",
		SummaryJSON: `{"summary":"existing"}`, Sealed: true, Provider: "fake",
		Model: "test", PromptVersion: "v1", NormalizerVersion: "v1",
	}
	if err := database.PutSummaryNode(ctx, node); err != nil {
		t.Fatal(err)
	}

	appendRecord := `{"timestamp":"2026-07-01T10:05:00Z","type":"event_msg","payload":{"type":"user_message","message":"Add one more test."}}` + "\n"
	if err := os.WriteFile(transcript, append(fixture, []byte(appendRecord)...), 0o600); err != nil {
		t.Fatal(err)
	}
	touchFuture(t, transcript)
	appended, err := scanner.Scan(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if appended.Imported != 1 {
		t.Fatalf("append report = %#v", appended)
	}
	if _, err := database.FindSummaryNode(ctx, cacheKey(node)); err != nil {
		t.Fatalf("sealed prefix was not reused after append: %v", err)
	}

	rewritten := strings.Replace(string(fixture), "Investigate the login timeout.", "Fix the login timeout.", 1)
	if err := os.WriteFile(transcript, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	touchFuture(t, transcript)
	changed, err := scanner.Scan(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if changed.Imported != 1 {
		t.Fatalf("rewrite report = %#v", changed)
	}
	if _, err := database.FindSummaryNode(ctx, cacheKey(node)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rewritten prefix node error = %v, want not found", err)
	}
}

func TestScannerRejectsOversizedTranscriptsBeforeRead(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	adapter := &oversizedSource{}
	scanner := source.NewScanner(database, adapter)
	report, err := scanner.Scan(context.Background(), "codex")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized scan report/error = %#v, %v", report, err)
	}
	if adapter.read.Load() {
		t.Fatal("oversized transcript was read")
	}
}

func TestScannerSerializesConcurrentScans(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	adapter := &blockingSource{entered: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	scanner := source.NewScanner(database, adapter)
	done := make(chan error, 2)
	go func() { _, err := scanner.Scan(context.Background(), "codex"); done <- err }()
	go func() { _, err := scanner.Scan(context.Background(), "codex"); done <- err }()
	<-adapter.entered
	select {
	case <-adapter.entered:
		t.Fatal("second scan entered while first scan was active")
	case <-time.After(30 * time.Millisecond):
	}
	adapter.release <- struct{}{}
	<-adapter.entered
	adapter.release <- struct{}{}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if adapter.maxActive.Load() != 1 {
		t.Fatalf("maximum concurrent scans = %d", adapter.maxActive.Load())
	}
}

type oversizedSource struct{ read atomic.Bool }

func (*oversizedSource) Name() string { return "codex" }
func (*oversizedSource) Discover(context.Context) ([]source.Candidate, error) {
	return []source.Candidate{{Agent: "codex", NativeSessionID: "oversized", Path: "/tmp/oversized", Size: source.MaxTranscriptBytes + 1}}, nil
}
func (s *oversizedSource) Read(context.Context, source.Candidate) (source.ImportedSession, error) {
	s.read.Store(true)
	return source.ImportedSession{}, nil
}
func (*oversizedSource) ResumeSpec(store.Session) (source.ResumeSpec, error) {
	return source.ResumeSpec{}, nil
}

type blockingSource struct {
	entered   chan struct{}
	release   chan struct{}
	active    atomic.Int32
	maxActive atomic.Int32
}

func (*blockingSource) Name() string { return "codex" }
func (s *blockingSource) Discover(ctx context.Context) ([]source.Candidate, error) {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		maximum := s.maxActive.Load()
		if active <= maximum || s.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	s.entered <- struct{}{}
	select {
	case <-s.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (*blockingSource) Read(context.Context, source.Candidate) (source.ImportedSession, error) {
	return source.ImportedSession{}, nil
}
func (*blockingSource) ResumeSpec(store.Session) (source.ResumeSpec, error) {
	return source.ResumeSpec{}, nil
}

func TestMixedCodexAndClaudeScanUsesNormalizedStore(t *testing.T) {
	ctx := context.Background()
	codexHome := t.TempDir()
	claudeHome := t.TempDir()
	codexPath := filepath.Join(codexHome, "sessions", "2026", "07", "rollout.jsonl")
	claudePath := filepath.Join(claudeHome, "projects", "-Users-example-work", "session.jsonl")
	copyFile(t, filepath.Join("codex", "testdata", "basic.jsonl"), codexPath)
	copyFile(t, filepath.Join("claude", "testdata", "basic.jsonl"), claudePath)

	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	scanner := source.NewScanner(database, codex.New(codexHome), claude.New(claudeHome))
	report, err := scanner.Scan(ctx, "all")
	if err != nil {
		t.Fatal(err)
	}
	if report.Discovered != 2 || report.Imported != 2 {
		t.Fatalf("mixed scan report = %#v", report)
	}

	codexID := source.StableID("codex", "11111111-1111-4111-8111-111111111111")
	claudeID := source.StableID("claude", "55555555-5555-4555-8555-555555555555")
	for id, wantAgent := range map[string]string{codexID: "codex", claudeID: "claude"} {
		detail, err := database.GetSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Session.Agent != wantAgent || len(detail.Messages) == 0 {
			t.Fatalf("session %q = agent %q, messages %d", id, detail.Session.Agent, len(detail.Messages))
		}
	}
}

func TestDeletedDatabaseRebuildsDeterministicallyFromSnapshot(t *testing.T) {
	ctx := context.Background()
	codexHome := t.TempDir()
	claudeHome := t.TempDir()
	copyFile(t, filepath.Join("codex", "testdata", "basic.jsonl"), filepath.Join(codexHome, "sessions", "2026", "07", "rollout.jsonl"))
	copyFile(t, filepath.Join("claude", "testdata", "basic.jsonl"), filepath.Join(claudeHome, "projects", "project", "session.jsonl"))

	build := func(path string) map[string]store.SessionDetail {
		database, err := store.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		scanner := source.NewScanner(database, codex.New(codexHome), claude.New(claudeHome))
		if _, err := scanner.Scan(ctx, "all"); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
		result := make(map[string]store.SessionDetail)
		for _, id := range []string{
			source.StableID("codex", "11111111-1111-4111-8111-111111111111"),
			source.StableID("claude", "55555555-5555-4555-8555-555555555555"),
		} {
			detail, err := database.GetSession(ctx, id)
			if err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			detail.Session.CreatedAt = time.Time{}
			detail.Session.UpdatedAt = time.Time{}
			result[id] = detail
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := build(filepath.Join(t.TempDir(), "first.db"))
	second := build(filepath.Join(t.TempDir(), "second.db"))
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("rebuilt normalized state differs\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func assertNoFTSMatch(t *testing.T, databasePath, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM session_fts WHERE session_fts MATCH ?`, query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("FTS match count for hidden text %q = %d", query, count)
	}
}

func cacheKey(node store.SummaryNode) store.NodeCacheKey {
	return store.NodeCacheKey{
		SessionID: node.SessionID, Kind: node.Kind, StartSequence: node.StartSequence,
		EndSequence: node.EndSequence, InputHash: node.InputHash, Provider: node.Provider,
		Model: node.Model, PromptVersion: node.PromptVersion, NormalizerVersion: node.NormalizerVersion,
	}
}

func touchFuture(t *testing.T, path string) {
	t.Helper()
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, sourcePath, destination string) {
	t.Helper()
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

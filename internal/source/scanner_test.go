package source_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/source/codex"
	"github.com/tarunjain/agent-history/internal/store"

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

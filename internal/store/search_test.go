package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchTextAndCombinedFilters(t *testing.T) {
	database := searchFixture(t)
	ctx := context.Background()

	tests := []struct {
		name  string
		query SearchQuery
		want  []string
	}{
		{name: "generated concept", query: SearchQuery{Text: "idempotency ledger"}, want: []string{"s1"}},
		{name: "analyzed transcript excluded", query: SearchQuery{Text: "database is locked"}, want: nil},
		{name: "analyzed transcript included", query: SearchQuery{Text: "database is locked", IncludeMessages: true}, want: []string{"s2"}},
		{name: "analyzed filename excluded", query: SearchQuery{Text: "internal/auth.go"}, want: nil},
		{name: "analyzed filename included", query: SearchQuery{Text: "internal/auth.go", IncludeMessages: true}, want: []string{"s1"}},
		{name: "unicode generated title", query: SearchQuery{Text: "café retry"}, want: []string{"s3"}},
		{name: "literal analyzed transcript excluded", query: SearchQuery{Text: ":"}, want: nil},
		{name: "literal analyzed transcript included", query: SearchQuery{Text: ":", IncludeMessages: true}, want: []string{"s2"}},
		{name: "unanalyzed message fallback", query: SearchQuery{Text: "archive old ledger"}, want: []string{"s4"}},
		{name: "agent folder multiple", query: SearchQuery{Agent: "codex", CWD: "payments", TopicMode: "multiple", AnalysisStatus: "current"}, want: []string{"s1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := database.SearchSessions(ctx, test.query)
			if err != nil {
				t.Fatal(err)
			}
			if got := hitIDs(result.Hits); fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Fatalf("hit IDs = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSearchFocusedScopeIncludesOnlyUnsummarizedTail(t *testing.T) {
	database := searchFixture(t)
	ctx := context.Background()
	detail, err := database.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	session := detail.Session
	session.SourceSize++
	session.SourceHash = "h1-appended"
	session.SourceMTime = session.SourceMTime.Add(time.Minute)
	session.LastActiveAt = session.LastActiveAt.Add(time.Minute)
	messages := append([]Message(nil), detail.Messages...)
	messages = append(messages, Message{
		Sequence: 2, Timestamp: session.LastActiveAt, Role: "assistant",
		Text: "Unsummarized zircon checksum result",
	})
	if _, err := database.ImportSession(ctx, session, messages); err != nil {
		t.Fatal(err)
	}

	assertSearchQueryIDs(t, database, SearchQuery{Text: "zircon checksum"}, []string{"s1"})
	assertSearchQueryIDs(t, database, SearchQuery{Text: "internal auth"}, nil)
	assertSearchQueryIDs(t, database, SearchQuery{Text: "internal auth", IncludeMessages: true}, []string{"s1"})
}

func TestSearchFocusedScopeUsesStoredAnalysisProvenance(t *testing.T) {
	for _, status := range []string{"queued", "running", "failed"} {
		t.Run(status, func(t *testing.T) {
			database := searchFixture(t)
			ctx := context.Background()
			if err := database.SetAnalysisStatus(ctx, "s2", status, "retry later"); err != nil {
				t.Fatal(err)
			}
			assertSearchQueryIDs(t, database, SearchQuery{Text: "database is locked"}, nil)

			if err := database.SetAnalysisStatus(ctx, "s4", status, "not analyzed"); err != nil {
				t.Fatal(err)
			}
			assertSearchQueryIDs(t, database, SearchQuery{Text: "archive old ledger"}, []string{"s4"})
		})
	}
}

func TestSearchKeywordsExcludeWorkingDirectoryColumn(t *testing.T) {
	database := searchFixture(t)
	assertSearchQueryIDs(t, database, SearchQuery{Text: "work search"}, nil)
	assertSearchQueryIDs(t, database, SearchQuery{Text: "work search", IncludeMessages: true}, nil)
	assertSearchQueryIDs(t, database, SearchQuery{CWD: "work/search"}, []string{"s2"})
}

func TestSearchFTSQueryPlanDoesNotRescanMessagesPerHit(t *testing.T) {
	database := searchFixture(t)
	rows, err := database.db.Query(
		"EXPLAIN QUERY PLAN "+textMatchesQuery,
		`{title body} : ("ledger"*)`,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "search_message") {
			t.Fatalf("FTS query plan rescans messages for each hit: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestSearchMalformedMessageKeyCannotQualifyAsUnsummarizedTail(t *testing.T) {
	database := searchFixture(t)
	if _, err := database.db.Exec(`
        INSERT INTO session_fts(session_id, document_key, document_type, title, body, working_directory)
        VALUES ('s1', 'message:2junk', 'message', '', 'malformed tail sentinel', '/work/payments')`); err != nil {
		t.Fatal(err)
	}
	assertSearchQueryIDs(t, database, SearchQuery{Text: "malformed tail sentinel"}, nil)
	assertSearchQueryIDs(t, database, SearchQuery{Text: "malformed tail sentinel", IncludeMessages: true}, []string{"s1"})
}

func TestSearchWeightsGeneratedTitlesAboveRawMessages(t *testing.T) {
	database := searchFixture(t)
	result, err := database.SearchSessions(context.Background(), SearchQuery{Text: "ledger"})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(result.Hits); fmt.Sprint(got) != fmt.Sprint([]string{"s1", "s4"}) {
		t.Fatalf("weighted ledger hits = %v, want title hit before raw hit", got)
	}
}

func TestSearchExplicitSorts(t *testing.T) {
	database := searchFixture(t)
	started, err := database.SearchSessions(context.Background(), SearchQuery{Sort: "started"})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(started.Hits); fmt.Sprint(got) != fmt.Sprint([]string{"s1", "s2", "s3", "s4"}) {
		t.Fatalf("started sort = %v", got)
	}
	titled, err := database.SearchSessions(context.Background(), SearchQuery{Sort: "title"})
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(titled.Hits); fmt.Sprint(got) != fmt.Sprint([]string{"s3", "s4", "s1", "s2"}) {
		t.Fatalf("title sort = %v", got)
	}
}

func TestSearchDateFiltersAndStableCursorPagination(t *testing.T) {
	database := searchFixture(t)
	ctx := context.Background()
	after := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	result, err := database.SearchSessions(ctx, SearchQuery{ActiveAfter: &after, Sort: "last_active", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.NextCursor == "" {
		t.Fatalf("first page = %#v", result)
	}
	seen := map[string]bool{result.Hits[0].Session.ID: true}
	cursor := result.NextCursor
	for cursor != "" {
		page, err := database.SearchSessions(ctx, SearchQuery{ActiveAfter: &after, Sort: "last_active", Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range page.Hits {
			if seen[hit.Session.ID] {
				t.Fatalf("duplicate paginated session %q", hit.Session.ID)
			}
			seen[hit.Session.ID] = true
		}
		cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("paginated sessions = %v, want 3", seen)
	}
}

func TestDeleteAndReanalysisUpdateSearchAtomically(t *testing.T) {
	database := searchFixture(t)
	ctx := context.Background()
	assertSearchIDs(t, database, "idempotency", []string{"s1"})
	if err := database.DeleteAnalysis(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	assertSearchIDs(t, database, "idempotency", nil)
	assertSearchIDs(t, database, "internal auth", []string{"s1"})

	session, err := database.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	analyzedThrough := session.Messages[len(session.Messages)-1].Sequence
	if err := database.ReplaceAnalysis(ctx, "s1", Analysis{
		Title: "Retry ledger", Summary: "Introduced a deduplication ledger.", Status: "current",
		Provider: "fake", Model: "test", PromptVersion: "v2", AnalyzedAt: time.Now(),
		AnalyzedHash: "new", AnalyzedThroughSequence: &analyzedThrough,
		AnalyzedThroughAt: session.Messages[len(session.Messages)-1].Timestamp,
		Segments:          []Segment{{Position: 0, StartSequence: 0, EndSequence: analyzedThrough, Title: "Deduplication", Summary: "Added the ledger.", Detail: "Verified retry behavior."}},
	}); err != nil {
		t.Fatal(err)
	}
	assertSearchIDs(t, database, "deduplication ledger", []string{"s1"})
}

func TestFacets(t *testing.T) {
	database := searchFixture(t)
	facets, err := database.SessionFacets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if facets.Agents["codex"] != 2 || facets.Agents["claude"] != 2 {
		t.Fatalf("agent facets = %#v", facets.Agents)
	}
	if facets.AnalysisStates["current"] != 3 || facets.AnalysisStates["none"] != 1 {
		t.Fatalf("analysis facets = %#v", facets.AnalysisStates)
	}
	if len(facets.Directories) == 0 || facets.StartedMin.IsZero() || facets.LastActiveMax.IsZero() {
		t.Fatalf("facets missing directory/date bounds: %#v", facets)
	}
}

func TestSearchRejectsInvalidOptions(t *testing.T) {
	database := searchFixture(t)
	for _, query := range []SearchQuery{
		{Agent: "other"}, {TopicMode: "unknown"}, {AnalysisStatus: "bad"},
		{Cmux: "unknown"}, {Sort: "random"}, {Limit: 501}, {Cursor: "not-a-cursor"},
	} {
		if _, err := database.SearchSessions(context.Background(), query); err == nil {
			t.Errorf("SearchSessions(%#v) succeeded, want validation error", query)
		}
	}
}

func TestSearchSessionsFiltersCmuxOpenAndClosed(t *testing.T) {
	database := searchFixture(t)
	ctx := context.Background()
	observedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	if err := database.ReplaceCmuxSnapshot(ctx, CmuxStatus{Available: true, AccessMode: "allowAll", ObservedAt: observedAt}, []CmuxSessionState{
		{SessionID: "s1", Open: true, WorkspaceID: "w1", SurfaceID: "p1", WorkspaceTitle: "one", SurfaceTitle: "one", Lifecycle: "running", ObservedAt: observedAt},
		{SessionID: "s2", Open: true, WorkspaceID: "w2", SurfaceID: "p2", WorkspaceTitle: "two", SurfaceTitle: "two", Lifecycle: "idle", ObservedAt: observedAt},
	}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		query SearchQuery
		want  []string
	}{
		{query: SearchQuery{Cmux: "open"}, want: []string{"s1", "s2"}},
		{query: SearchQuery{Cmux: "closed"}, want: []string{"s3", "s4"}},
		{query: SearchQuery{Cmux: "open", Agent: "claude"}, want: []string{"s2"}},
	}
	for _, test := range tests {
		result, err := database.SearchSessions(ctx, test.query)
		if err != nil {
			t.Fatal(err)
		}
		if got := hitIDs(result.Hits); fmt.Sprint(got) != fmt.Sprint(test.want) {
			t.Fatalf("SearchSessions(%#v) = %v, want %v", test.query, got, test.want)
		}
	}
}

func searchFixture(t *testing.T) *Store {
	t.Helper()
	database, err := Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	sessions := []struct {
		session  Session
		messages []Message
		analysis *Analysis
	}{
		{
			session:  Session{ID: "s1", Agent: "codex", NativeSessionID: "n1", SourcePath: "/tmp/s1", SourceSize: 1, SourceMTime: base, SourceHash: "h1", WorkingDirectory: "/work/payments", StartedAt: base, LastActiveAt: base.Add(time.Hour)},
			messages: []Message{{Sequence: 0, Timestamp: base, Role: "user", Text: "Fix internal/auth.go retry behavior"}, {Sequence: 1, Timestamp: base.Add(time.Hour), Role: "assistant", Text: "Tests pass"}},
			analysis: &Analysis{Title: "Payment idempotency ledger", Summary: "Built an idempotency ledger for retries.", Status: "current", Provider: "fake", Model: "test", PromptVersion: "v1", AnalyzedAt: base.Add(time.Hour), AnalyzedHash: "a1", AnalyzedThroughSequence: intPointer(1), AnalyzedThroughAt: base.Add(time.Hour), Segments: []Segment{{Position: 0, StartSequence: 0, EndSequence: 0, Title: "Investigate", Summary: "Found retry issue", Detail: "Inspected auth"}, {Position: 1, StartSequence: 1, EndSequence: 1, Title: "Implement", Summary: "Added ledger", Detail: "Verified tests"}}},
		},
		{
			session:  Session{ID: "s2", Agent: "claude", NativeSessionID: "n2", SourcePath: "/tmp/s2", SourceSize: 1, SourceMTime: base, SourceHash: "h2", WorkingDirectory: "/work/search", StartedAt: base.Add(-24 * time.Hour), LastActiveAt: base.Add(time.Hour)},
			messages: []Message{{Sequence: 0, Timestamp: base, Role: "tool", ToolName: "sqlite", Text: "error: database is locked while writing"}},
			analysis: &Analysis{Title: "SQLite locking", Summary: "Investigated writer contention.", Status: "current", Provider: "fake", Model: "test", PromptVersion: "v1", AnalyzedAt: base, AnalyzedHash: "a2", AnalyzedThroughSequence: intPointer(0), AnalyzedThroughAt: base, Segments: []Segment{{Position: 0, StartSequence: 0, EndSequence: 0, Title: "Locking", Summary: "Found contention", Detail: "One writer"}}},
		},
		{
			session:  Session{ID: "s3", Agent: "codex", NativeSessionID: "n3", SourcePath: "/tmp/s3", SourceSize: 1, SourceMTime: base, SourceHash: "h3", WorkingDirectory: "/work/cafe", StartedAt: base.Add(-48 * time.Hour), LastActiveAt: base.Add(-time.Hour)},
			messages: []Message{{Sequence: 0, Timestamp: base.Add(-time.Hour), Role: "user", Text: "Fix the café retry labels"}},
			analysis: &Analysis{Title: "Café retry labels", Summary: "Corrected localized labels.", Status: "current", Provider: "fake", Model: "test", PromptVersion: "v1", AnalyzedAt: base, AnalyzedHash: "a3", AnalyzedThroughSequence: intPointer(0), AnalyzedThroughAt: base, Segments: []Segment{{Position: 0, StartSequence: 0, EndSequence: 0, Title: "Labels", Summary: "Fixed labels", Detail: "Unicode retained"}}},
		},
		{
			session:  Session{ID: "s4", Agent: "claude", NativeSessionID: "n4", SourcePath: "/tmp/s4", SourceSize: 1, SourceMTime: base, SourceHash: "h4", WorkingDirectory: "/work/archive", StartedAt: base.Add(-365 * 24 * time.Hour), LastActiveAt: base.Add(-300 * 24 * time.Hour)},
			messages: []Message{{Sequence: 0, Timestamp: base.Add(-300 * 24 * time.Hour), Role: "user", Text: "Archive old ledger notes"}},
		},
	}
	for _, fixture := range sessions {
		if _, err := database.ImportSession(context.Background(), fixture.session, fixture.messages); err != nil {
			t.Fatal(err)
		}
		if fixture.analysis != nil {
			if err := database.ReplaceAnalysis(context.Background(), fixture.session.ID, *fixture.analysis); err != nil {
				t.Fatal(err)
			}
		}
	}
	return database
}

func assertSearchIDs(t *testing.T, database *Store, query string, want []string) {
	t.Helper()
	assertSearchQueryIDs(t, database, SearchQuery{Text: query}, want)
}

func assertSearchQueryIDs(t *testing.T, database *Store, query SearchQuery, want []string) {
	t.Helper()
	result, err := database.SearchSessions(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(result.Hits); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("SearchSessions(%#v) IDs = %v, want %v", query, got, want)
	}
}

func hitIDs(hits []SessionHit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.Session.ID)
	}
	return ids
}

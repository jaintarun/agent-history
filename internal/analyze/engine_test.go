package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaintarun/agent-history/internal/store"
)

func TestEngineBuildsThreeLevelsAndReusesUnaffectedTree(t *testing.T) {
	database, session := analysisFixture(t)
	fake := &fakeAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"fake": fake})
	options := Options{
		Provider: "fake", Model: "test-model", PromptVersion: "v1",
		NormalizerVersion: "v1", LeafTargetChars: 10_000, RollupFanout: 8,
	}

	result, err := engine.Analyze(context.Background(), session.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Calls != 3 {
		t.Fatalf("initial analyzer calls = %d, want 3", result.Calls)
	}
	detail, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.Title != "Focused development session" || len(detail.Segments) != 2 {
		t.Fatalf("stored analysis = title %q, segments %d", detail.Session.Title, len(detail.Segments))
	}
	if detail.Segments[0].StartSequence != 0 || detail.Segments[0].EndSequence != 1 ||
		detail.Segments[1].StartSequence != 2 || detail.Segments[1].EndSequence != 3 {
		t.Fatalf("segment ranges = %#v", detail.Segments)
	}
	if detail.Session.AnalysisProvider != "fake" || detail.Session.Agent != "claude" {
		t.Fatalf("source/provider were conflated: %#v", detail.Session)
	}

	appended := append(analysisMessages(),
		store.Message{Sequence: 4, Timestamp: session.LastActiveAt.Add(time.Hour), Role: "user", Text: "Add filters to that vault."},
		store.Message{Sequence: 5, Timestamp: session.LastActiveAt.Add(time.Hour + time.Minute), Role: "assistant", Text: "The vault filters are implemented."},
	)
	session.SourceSize++
	session.SourceHash = "appended"
	session.LastActiveAt = appended[len(appended)-1].Timestamp
	if _, err := database.ImportSession(context.Background(), session, appended); err != nil {
		t.Fatal(err)
	}
	partial, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Session.AnalysisStatus != "partial" || partial.Session.Title == "" {
		t.Fatalf("appended session status/title = %q, %q", partial.Session.AnalysisStatus, partial.Session.Title)
	}
	fake.reset()

	result, err = engine.Analyze(context.Background(), session.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Calls != 3 {
		t.Fatalf("append analyzer calls = %d, want 3", result.Calls)
	}
	for _, request := range fake.requestsCopy() {
		if strings.Contains(request.Prompt, "Fix authentication") || strings.Contains(request.Prompt, "redesign the vault") {
			t.Fatalf("append request resent sealed raw history in %s prompt:\n%s", request.Kind, request.Prompt)
		}
	}
}

func TestEnginePromotesSingleLeafWithoutRedundantModelCalls(t *testing.T) {
	database, session := analysisFixture(t)
	messages := analysisMessages()[:2]
	session.SourceHash = "single-topic"
	session.LastActiveAt = messages[len(messages)-1].Timestamp
	if _, err := database.ImportSession(context.Background(), session, messages); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"fake": fake})
	result, err := engine.Analyze(context.Background(), session.ID, Options{
		Provider: "fake", Model: "test", PromptVersion: "v1",
		NormalizerVersion: "v2", LeafTargetChars: 48_000, RollupFanout: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Calls != 1 || len(result.Analysis.Topics) != 1 || result.Analysis.Title != "Leaf" {
		t.Fatalf("single-leaf result = %#v", result)
	}
	detail, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	nodeCount := 0
	for _, kind := range []string{"leaf", "topic", "session"} {
		nodes, err := database.SummaryNodes(context.Background(), session.ID, kind, "fake", "test", "v1", "v2")
		if err != nil {
			t.Fatal(err)
		}
		nodeCount += len(nodes)
	}
	if len(detail.Segments) != 1 || nodeCount != 3 {
		t.Fatalf("persisted segments/nodes = %d/%d", len(detail.Segments), nodeCount)
	}
}

func TestEngineRetitleUsesStoredTopicsAndChangesOnlyTitle(t *testing.T) {
	database, session := analysisFixture(t)
	fake := &fakeAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"fake": fake})
	options := Options{
		Provider: "fake", Model: "test", PromptVersion: "v1",
		NormalizerVersion: "v1", LeafTargetChars: 10_000, RollupFanout: 8,
	}
	if _, err := engine.Analyze(context.Background(), session.ID, options); err != nil {
		t.Fatal(err)
	}
	before, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake.reset()
	fake.responses = map[RequestKind]json.RawMessage{
		RequestTitle: json.RawMessage(`{"title":"Authentication repair and searchable vault redesign outcomes"}`),
	}

	title, err := engine.Retitle(context.Background(), session.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Authentication repair and searchable vault redesign outcomes" {
		t.Fatalf("retitled title = %q", title)
	}
	requests := fake.requestsCopy()
	if len(requests) != 1 || requests[0].Kind != RequestTitle ||
		!strings.Contains(requests[0].Prompt, before.Segments[0].Title) ||
		strings.Contains(requests[0].Prompt, "Fix authentication") {
		t.Fatalf("retitle requests = %#v", requests)
	}
	after, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Session.Summary != before.Session.Summary || len(after.Segments) != len(before.Segments) || after.Session.AnalysisStatus != "current" {
		t.Fatalf("retitle changed non-title analysis: before=%#v after=%#v", before, after)
	}
	nodes, err := database.SummaryNodes(context.Background(), session.ID, "session", "fake", "test", "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || !strings.Contains(nodes[0].SummaryJSON, title) {
		t.Fatalf("retitled session nodes = %#v", nodes)
	}
}

func TestEngineRetitleFailurePreservesAnalysisAndIsMarkedDistinctly(t *testing.T) {
	database, session := analysisFixture(t)
	fake := &fakeAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"fake": fake})
	options := Options{
		Provider: "fake", Model: "test", PromptVersion: "v1",
		NormalizerVersion: "v1", LeafTargetChars: 10_000, RollupFanout: 8,
	}
	if _, err := engine.Analyze(context.Background(), session.ID, options); err != nil {
		t.Fatal(err)
	}
	before, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake.reset()
	fake.responses = map[RequestKind]json.RawMessage{RequestTitle: json.RawMessage(`{"title":"Too short"}`)}

	if _, err := engine.Retitle(context.Background(), session.ID, options); err == nil {
		t.Fatal("short retitle succeeded")
	}
	after, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Session.Title != before.Session.Title || after.Session.Summary != before.Session.Summary ||
		after.Session.AnalysisStatus != "failed" || !strings.HasPrefix(after.Session.AnalysisError, "retitle:") {
		t.Fatalf("analysis after failed retitle = %#v", after.Session)
	}
	ids, err := database.PendingAnalysisSessionIDs(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == session.ID {
			t.Fatalf("failed retitle was queued as full analysis: %v", ids)
		}
	}
}

func TestEngineLeafRangesCoverMessagesWithoutOverlap(t *testing.T) {
	database, session := analysisFixture(t)
	fake := &fakeAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"fake": fake})
	options := Options{Provider: "fake", Model: "test", PromptVersion: "v1", NormalizerVersion: "v1", LeafTargetChars: 80, RollupFanout: 8}
	if _, err := engine.Analyze(context.Background(), session.ID, options); err != nil {
		t.Fatal(err)
	}
	nodes, err := database.SummaryNodes(context.Background(), session.ID, "leaf", "fake", "test", "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	covered := make(map[int]int)
	for _, node := range nodes {
		for sequence := node.StartSequence; sequence <= node.EndSequence; sequence++ {
			covered[sequence]++
		}
	}
	for _, message := range analysisMessages() {
		if covered[message.Sequence] != 1 {
			t.Fatalf("message %d leaf membership = %d, want 1", message.Sequence, covered[message.Sequence])
		}
	}
}

func TestEngineUsesBoundedFanoutForLargeTopic(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	var messages []store.Message
	for step := 0; step < 5; step++ {
		messages = append(messages,
			store.Message{Sequence: step * 2, Timestamp: start.Add(time.Duration(step) * time.Minute), Role: "user", Text: "Continue the vault implementation with the next small step."},
			store.Message{Sequence: step*2 + 1, Timestamp: start.Add(time.Duration(step)*time.Minute + time.Second), Role: "assistant", Text: "The vault implementation step is complete and verified."},
		)
	}
	session := store.Session{
		ID: "large-topic", Agent: "codex", NativeSessionID: "large-topic-native",
		SourcePath: "/tmp/large.jsonl", SourceSize: 1, SourceMTime: start,
		SourceHash: "large", WorkingDirectory: "/work/vault",
		StartedAt: start, LastActiveAt: messages[len(messages)-1].Timestamp,
	}
	if _, err := database.ImportSession(context.Background(), session, messages); err != nil {
		t.Fatal(err)
	}
	fake := &fakeAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"fake": fake})
	options := Options{Provider: "fake", Model: "test", PromptVersion: "v1", NormalizerVersion: "v1", LeafTargetChars: 100, RollupFanout: 2}
	if _, err := engine.Analyze(context.Background(), session.ID, options); err != nil {
		t.Fatal(err)
	}
	rollups, err := database.SummaryNodes(context.Background(), session.ID, "rollup", "fake", "test", "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rollups) == 0 {
		t.Fatal("large topic produced no bounded-fanout rollup nodes")
	}
	detail, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Segments) != 1 {
		t.Fatalf("large coherent topic segments = %d, want 1", len(detail.Segments))
	}
}

func TestEngineModelChangeInvalidatesCache(t *testing.T) {
	database, session := analysisFixture(t)
	fake := &fakeAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"fake": fake})
	options := Options{Provider: "fake", Model: "one", PromptVersion: "v1", NormalizerVersion: "v1", LeafTargetChars: 10_000, RollupFanout: 8}
	if _, err := engine.Analyze(context.Background(), session.ID, options); err != nil {
		t.Fatal(err)
	}
	fake.reset()
	options.Model = "two"
	result, err := engine.Analyze(context.Background(), session.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Calls != 3 {
		t.Fatalf("new-model calls = %d, want full tree 3", result.Calls)
	}
}

func TestEngineFailurePreservesPreviousAnalysis(t *testing.T) {
	database, session := analysisFixture(t)
	fake := &fakeAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"fake": fake})
	options := Options{Provider: "fake", Model: "one", PromptVersion: "v1", NormalizerVersion: "v1", LeafTargetChars: 10_000, RollupFanout: 8}
	if _, err := engine.Analyze(context.Background(), session.ID, options); err != nil {
		t.Fatal(err)
	}
	fake.reset()
	fake.err = errors.New("analyzer unavailable")
	options.Model = "two"
	if _, err := engine.Analyze(context.Background(), session.ID, options); err == nil {
		t.Fatal("Analyze succeeded, want error")
	}
	detail, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.Title != "Focused development session" || len(detail.Segments) != 2 {
		t.Fatalf("previous analysis was lost: %#v", detail)
	}
	if detail.Session.AnalysisStatus != "failed" || !strings.Contains(detail.Session.AnalysisError, "analyzer unavailable") {
		t.Fatalf("failure status = %q, %q", detail.Session.AnalysisStatus, detail.Session.AnalysisError)
	}
}

func TestEngineRejectsSchemaMismatchAndPreservesPreviousAnalysis(t *testing.T) {
	database, session := analysisFixture(t)
	fake := &fakeAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"fake": fake})
	options := Options{Provider: "fake", Model: "one", PromptVersion: "v1", NormalizerVersion: "v1", LeafTargetChars: 10_000, RollupFanout: 8}
	if _, err := engine.Analyze(context.Background(), session.ID, options); err != nil {
		t.Fatal(err)
	}
	fake.reset()
	fake.responses = map[RequestKind]json.RawMessage{
		RequestLeaf: json.RawMessage(`{"title":"missing required fields"}`),
	}
	options.Model = "two"
	if _, err := engine.Analyze(context.Background(), session.ID, options); err == nil {
		t.Fatal("Analyze accepted schema-mismatched leaf")
	}
	detail, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.Title != "Focused development session" || len(detail.Segments) != 2 {
		t.Fatalf("previous analysis was replaced after schema mismatch: %#v", detail)
	}
}

func TestBoundaryPromptContainsOnlyBoundedAdjacentContext(t *testing.T) {
	before := &leafPlan{lastTurn: Turn{Projection: strings.Repeat("a", 3_000) + "BEFORE_TAIL"}}
	after := &leafPlan{firstTurn: Turn{Projection: strings.Repeat("b", 3_000) + "AFTER_TAIL"}}

	prompt := boundaryPrompt(before, after)

	if len(prompt) > 4_000 {
		t.Fatalf("boundary prompt length = %d, want bounded", len(prompt))
	}
	if strings.Contains(prompt, "BEFORE_TAIL") || strings.Contains(prompt, "AFTER_TAIL") {
		t.Fatalf("boundary prompt included non-adjacent tail data")
	}
}

type fakeAnalyzer struct {
	mu        sync.Mutex
	requests  []StructuredRequest
	err       error
	responses map[RequestKind]json.RawMessage
}

func (f *fakeAnalyzer) Generate(_ context.Context, _ string, request StructuredRequest) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.err != nil {
		return nil, f.err
	}
	if response, ok := f.responses[request.Kind]; ok {
		return response, nil
	}
	switch request.Kind {
	case RequestLeaf:
		return json.RawMessage(`{"title":"Leaf","goal":"Complete the task","summary":"Implemented one bounded part.","detail":"Work and verification were completed.","outcome":"complete","entities":[],"files":[],"errors":[],"evidence":[]}`), nil
	case RequestBoundary:
		return json.RawMessage(`{"same_topic":true,"confidence":0.8,"new_title":""}`), nil
	case RequestTopic, RequestRollup:
		return json.RawMessage(`{"title":"Topic","summary":"Completed a coherent topic.","detail":"The topic contains implementation and verification.","evidence":[]}`), nil
	case RequestSession:
		return json.RawMessage(`{"title":"Focused development session","summary":"Completed two chronological development topics."}`), nil
	case RequestTitle:
		return json.RawMessage(`{"title":"Focused development session with specific implementation outcomes"}`), nil
	default:
		return nil, errors.New("unexpected request kind")
	}
}

func (f *fakeAnalyzer) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
	f.err = nil
	f.responses = nil
}

func (f *fakeAnalyzer) requestsCopy() []StructuredRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StructuredRequest(nil), f.requests...)
}

func analysisFixture(t *testing.T) (*store.Store, store.Session) {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	messages := analysisMessages()
	session := store.Session{
		ID: "analysis-session", Agent: "claude", NativeSessionID: "native-analysis",
		SourcePath: "/tmp/analysis.jsonl", SourceSize: 100, SourceMTime: messages[0].Timestamp,
		SourceHash: "initial", WorkingDirectory: "/work/api",
		StartedAt: messages[0].Timestamp, LastActiveAt: messages[len(messages)-1].Timestamp,
		AnalysisStatus: "none",
	}
	if _, err := database.ImportSession(context.Background(), session, messages); err != nil {
		t.Fatal(err)
	}
	return database, session
}

func analysisMessages() []store.Message {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	return []store.Message{
		{Sequence: 0, Timestamp: start, Role: "user", Text: "Fix authentication in /work/api."},
		{Sequence: 1, Timestamp: start.Add(time.Minute), Role: "assistant", Text: "Authentication now passes its tests."},
		{Sequence: 2, Timestamp: start.Add(2 * time.Hour), Role: "user", Text: "New task: redesign the vault in /work/history."},
		{Sequence: 3, Timestamp: start.Add(2*time.Hour + time.Minute), Role: "assistant", Text: "The vault design is complete."},
	}
}

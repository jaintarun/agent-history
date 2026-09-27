package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jaintarun/agent-history/internal/store"
)

func TestWorkerSerializesAnalysisRequests(t *testing.T) {
	database, first := analysisFixture(t)
	second := first
	second.ID = "analysis-session-two"
	second.NativeSessionID = "native-analysis-two"
	second.SourcePath = "/tmp/analysis-two.jsonl"
	if _, err := database.ImportSession(context.Background(), second, analysisMessages()); err != nil {
		t.Fatal(err)
	}
	analyzer := &trackingAnalyzer{}
	engine := NewEngine(database, map[string]Analyzer{"tracking": analyzer})
	worker := NewWorker(database, engine, 2)
	t.Cleanup(worker.Close)
	options := Options{Provider: "tracking", Model: "test", PromptVersion: "v1", NormalizerVersion: "v1", LeafTargetChars: 10_000, RollupFanout: 8}

	firstDone, err := worker.Enqueue(context.Background(), first.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Enqueue(context.Background(), first.ID, options); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("duplicate enqueue error = %v, want ErrAlreadyQueued", err)
	}
	secondDone, err := worker.Enqueue(context.Background(), second.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if analyzer.maxActive != 1 {
		t.Fatalf("maximum concurrent analyzer calls = %d, want 1", analyzer.maxActive)
	}
	for _, id := range []string{first.ID, second.ID} {
		detail, err := database.GetSession(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Session.AnalysisStatus != "current" {
			t.Fatalf("session %q status = %q", id, detail.Session.AnalysisStatus)
		}
	}
}

func TestWorkerRejectsSessionWithoutVisibleMessages(t *testing.T) {
	database, _ := analysisFixture(t)
	empty := store.Session{
		ID: "empty-session", Agent: "claude", NativeSessionID: "empty-native",
		SourcePath: "/tmp/empty.jsonl", SourceSize: 1, SourceMTime: time.Now(),
		SourceHash: "empty", WorkingDirectory: "/tmp", StartedAt: time.Now(), LastActiveAt: time.Now(),
	}
	if err := database.UpsertSession(context.Background(), empty); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(database, NewEngine(database, map[string]Analyzer{"tracking": &trackingAnalyzer{}}), 1)
	t.Cleanup(worker.Close)
	_, err := worker.Enqueue(context.Background(), empty.ID, Options{Provider: "tracking", Model: "test", PromptVersion: "v1", NormalizerVersion: "v1"})
	if !errors.Is(err, ErrNoVisibleMessages) {
		t.Fatalf("empty enqueue error = %v", err)
	}
	detail, err := database.GetSession(context.Background(), empty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.AnalysisStatus != "none" {
		t.Fatalf("empty session status = %q", detail.Session.AnalysisStatus)
	}
}

func TestWorkerFullQueueLeavesSessionPending(t *testing.T) {
	database, first := analysisFixture(t)
	for _, id := range []string{"second", "third"} {
		session := first
		session.ID = id
		session.NativeSessionID = "native-" + id
		session.SourcePath = "/tmp/" + id + ".jsonl"
		if _, err := database.ImportSession(context.Background(), session, analysisMessages()); err != nil {
			t.Fatal(err)
		}
	}
	analyzer := &blockingAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	worker := NewWorker(database, NewEngine(database, map[string]Analyzer{"tracking": analyzer}), 1)
	t.Cleanup(worker.Close)
	options := Options{Provider: "tracking", Model: "test", PromptVersion: "v1", NormalizerVersion: "v1", LeafTargetChars: 10_000, RollupFanout: 8}
	if _, err := worker.Enqueue(context.Background(), first.ID, options); err != nil {
		t.Fatal(err)
	}
	<-analyzer.started
	if _, err := worker.Enqueue(context.Background(), "second", options); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := worker.Enqueue(ctx, "third", options); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full queue error = %v, want ErrQueueFull", err)
	}
	detail, err := database.GetSession(context.Background(), "third")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.AnalysisStatus != "none" {
		t.Fatalf("third session status = %q, want none", detail.Session.AnalysisStatus)
	}
	close(analyzer.release)
}

type blockingAnalyzer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	tracker trackingAnalyzer
}

func (a *blockingAnalyzer) Generate(ctx context.Context, model string, request StructuredRequest) (json.RawMessage, error) {
	a.once.Do(func() { close(a.started) })
	select {
	case <-a.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return a.tracker.Generate(ctx, model, request)
}

func TestWorkerQueuesRetitleAfterAnalysis(t *testing.T) {
	database, session := analysisFixture(t)
	analyzer := &trackingAnalyzer{}
	worker := NewWorker(database, NewEngine(database, map[string]Analyzer{"tracking": analyzer}), 2)
	t.Cleanup(worker.Close)
	options := Options{
		Provider: "tracking", Model: "test", PromptVersion: "v1",
		NormalizerVersion: "v1", LeafTargetChars: 10_000, RollupFanout: 8,
	}
	if _, err := worker.EnqueueRetitle(context.Background(), session.ID, options); !errors.Is(err, ErrNoTopics) {
		t.Fatalf("retitle without topics error = %v", err)
	}
	done, err := worker.Enqueue(context.Background(), session.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	done, err = worker.EnqueueRetitle(context.Background(), session.ID, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	detail, err := database.GetSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.Title != "Specific retitled development session with implementation outcomes" || detail.Session.AnalysisStatus != "current" {
		t.Fatalf("retitled session = %#v", detail.Session)
	}
}

type trackingAnalyzer struct {
	mu        sync.Mutex
	active    int
	maxActive int
}

func (a *trackingAnalyzer) Generate(_ context.Context, _ string, request StructuredRequest) (json.RawMessage, error) {
	a.mu.Lock()
	a.active++
	if a.active > a.maxActive {
		a.maxActive = a.active
	}
	a.mu.Unlock()
	time.Sleep(time.Millisecond)
	a.mu.Lock()
	a.active--
	a.mu.Unlock()
	switch request.Kind {
	case RequestLeaf:
		return json.RawMessage(`{"title":"Leaf","goal":"Goal","summary":"Summary","detail":"Detail","outcome":"done","entities":[],"files":[],"errors":[],"evidence":[]}`), nil
	case RequestTopic, RequestRollup:
		return json.RawMessage(`{"title":"Topic","summary":"Summary","detail":"Detail","evidence":[]}`), nil
	case RequestBoundary:
		return json.RawMessage(`{"same_topic":true,"confidence":1,"new_title":""}`), nil
	case RequestTitle:
		return json.RawMessage(`{"title":"Specific retitled development session with implementation outcomes"}`), nil
	default:
		return json.RawMessage(`{"title":"Session","summary":"Summary"}`), nil
	}
}

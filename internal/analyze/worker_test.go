package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
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
	default:
		return json.RawMessage(`{"title":"Session","summary":"Summary"}`), nil
	}
}

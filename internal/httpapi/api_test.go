package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tarunjain/agent-history/internal/analyze"
	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/store"
)

func TestSecurityMiddleware(t *testing.T) {
	handler, _, _, _, _, _ := testHandler(t)

	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.Host = "127.0.0.1:1234"
	if response := serve(handler, request); response.Code != http.StatusNotFound {
		t.Fatalf("missing token status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/test-token/api/health", nil)
	request.Host = "example.com"
	if response := serve(handler, request); response.Code != http.StatusForbidden {
		t.Fatalf("non-loopback Host status = %d", response.Code)
	}

	request = jsonRequest(http.MethodPost, "/test-token/api/scan", `{}`)
	request.Header.Set("Origin", "https://evil.example")
	if response := serve(handler, request); response.Code != http.StatusForbidden {
		t.Fatalf("invalid Origin status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/test-token/api/health", nil)
	request.Host = "127.0.0.1:1234"
	if response := serve(handler, request); response.Code != http.StatusOK {
		t.Fatalf("health status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestSessionReadEndpoints(t *testing.T) {
	handler, _, _, _, _, _ := testHandler(t)

	response := serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions?q=login&agent=codex", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("search status = %d body=%s", response.Code, response.Body.String())
	}
	var search struct {
		Sessions []struct {
			ID    string `json:"id"`
			Agent string `json:"agent"`
		} `json:"sessions"`
	}
	decodeResponse(t, response, &search)
	if len(search.Sessions) != 1 || search.Sessions[0].ID != "session-1" || search.Sessions[0].Agent != "codex" {
		t.Fatalf("search response = %#v", search)
	}

	response = serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/session-1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", response.Code, response.Body.String())
	}
	var detail map[string]any
	decodeResponse(t, response, &detail)
	if detail["title"] != "Login retry investigation" || detail["span_seconds"].(float64) != 3600 {
		t.Fatalf("detail response = %#v", detail)
	}

	response = serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/session-1/messages?include_tools=false", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("messages status = %d body=%s", response.Code, response.Body.String())
	}
	var messages struct {
		Messages []map[string]any `json:"messages"`
	}
	decodeResponse(t, response, &messages)
	if len(messages.Messages) != 2 || messages.Messages[0]["role"] != "user" {
		t.Fatalf("messages response = %#v", messages)
	}

	response = serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/facets", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("facets status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestMutationEndpoints(t *testing.T) {
	handler, database, scanner, queue, launcher, _ := testHandler(t)

	response := serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/analyze", `{"model":"override-model","full":true}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("analyze status = %d body=%s", response.Code, response.Body.String())
	}
	queue.mu.Lock()
	if len(queue.jobs) != 1 || queue.jobs[0].sessionID != "session-1" || queue.jobs[0].options.Model != "override-model" || !queue.jobs[0].options.Full {
		t.Fatalf("queued jobs = %#v", queue.jobs)
	}
	queue.mu.Unlock()

	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/scan", `{"agent":"codex"}`))
	if response.Code != http.StatusOK || scanner.scanAgent != "codex" {
		t.Fatalf("scan response=%d scanner=%#v body=%s", response.Code, scanner, response.Body.String())
	}
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/rescan", `{}`))
	if response.Code != http.StatusOK || scanner.rescanID != "session-1" {
		t.Fatalf("rescan response=%d scanner=%#v", response.Code, scanner)
	}
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/launch", `{"launcher":"copy"}`))
	if response.Code != http.StatusOK || launcher.sessionID != "session-1" || launcher.mode != "copy" {
		t.Fatalf("launch response=%d launcher=%#v body=%s", response.Code, launcher, response.Body.String())
	}

	request := apiRequest(http.MethodDelete, "/test-token/api/sessions/session-1/analysis", nil)
	request.Header.Set("Origin", "http://127.0.0.1:1234")
	response = serve(handler, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete analysis status = %d body=%s", response.Code, response.Body.String())
	}
	detail, err := database.GetSession(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.Title != "" || len(detail.Messages) != 2 {
		t.Fatalf("detail after deletion = %#v", detail)
	}
}

func TestSettingsAreValidatedAndNeverExposeSecrets(t *testing.T) {
	handler, database, _, queue, _, _ := testHandler(t)
	if err := database.SetSetting(context.Background(), "analysis.model", "initial-model"); err != nil {
		t.Fatal(err)
	}

	response := serve(handler, apiRequest(http.MethodGet, "/test-token/api/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get settings status = %d", response.Code)
	}
	if strings.Contains(strings.ToLower(response.Body.String()), "key") || strings.Contains(strings.ToLower(response.Body.String()), "credential") {
		t.Fatalf("settings response appears to expose secret fields: %s", response.Body.String())
	}

	response = serve(handler, jsonRequest(http.MethodPut, "/test-token/api/settings", `{"analysis_provider":"codex-cli","analysis_model":"new-model","analysis_auto":false}`))
	if response.Code != http.StatusOK {
		t.Fatalf("put settings status = %d body=%s", response.Code, response.Body.String())
	}
	model, ok, err := database.Setting(context.Background(), "analysis.model")
	if err != nil || !ok || model != "new-model" {
		t.Fatalf("stored model = %q, %v, %v", model, ok, err)
	}
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/analyze", `{}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("analyze status = %d body=%s", response.Code, response.Body.String())
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.jobs) != 1 || queue.jobs[0].options.Model != "new-model" {
		t.Fatalf("analysis did not use saved model: %#v", queue.jobs)
	}
}

func TestJSONValidationErrorsAndNotFound(t *testing.T) {
	handler, _, _, queue, _, _ := testHandler(t)
	response := serve(handler, apiRequest(http.MethodPost, "/test-token/api/scan", strings.NewReader(`{}`)))
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type status = %d", response.Code)
	}
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/scan", `{broken`))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %d", response.Code)
	}
	response = serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing session status = %d", response.Code)
	}
	queue.err = errors.New("queue unavailable")
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/analyze", `{}`))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("queue error status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestAccessLogsDoNotContainTranscriptText(t *testing.T) {
	handler, _, _, _, _, logs := testHandler(t)
	response := serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/session-1/messages", nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	if strings.Contains(logs.String(), "sensitive transcript phrase") {
		t.Fatalf("access log contains transcript content: %s", logs.String())
	}
}

func TestMiddlewareRejectsOversizedBodiesAndRecoversPanics(t *testing.T) {
	handler, _, scanner, _, _, _ := testHandler(t)
	large := `{"agent":"` + strings.Repeat("x", maxJSONBody) + `"}`
	response := serve(handler, jsonRequest(http.MethodPost, "/test-token/api/scan", large))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d body=%s", response.Code, response.Body.String())
	}

	scanner.panic = true
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/scan", `{}`))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d body=%s", response.Code, response.Body.String())
	}
}

type fakeScanner struct {
	scanAgent string
	rescanID  string
	panic     bool
}

func (s *fakeScanner) Scan(_ context.Context, agent string) (source.ScanReport, error) {
	if s.panic {
		panic("test panic")
	}
	s.scanAgent = agent
	return source.ScanReport{Discovered: 1, Imported: 1}, nil
}

func (s *fakeScanner) Rescan(_ context.Context, sessionID string) (store.ImportResult, error) {
	s.rescanID = sessionID
	return store.ImportResult{Changed: true}, nil
}

type queuedJob struct {
	sessionID string
	options   analyze.Options
}

type fakeQueue struct {
	mu   sync.Mutex
	jobs []queuedJob
	err  error
}

func (q *fakeQueue) Enqueue(_ context.Context, sessionID string, options analyze.Options) (<-chan error, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return nil, q.err
	}
	q.jobs = append(q.jobs, queuedJob{sessionID: sessionID, options: options})
	done := make(chan error, 1)
	done <- nil
	close(done)
	return done, nil
}

type fakeLauncher struct {
	sessionID string
	mode      string
}

func (l *fakeLauncher) Launch(_ context.Context, sessionID, mode string) (LaunchResult, error) {
	l.sessionID, l.mode = sessionID, mode
	return LaunchResult{Mode: "copy", Command: "codex resume native-1"}, nil
}

func testHandler(t *testing.T) (http.Handler, *store.Store, *fakeScanner, *fakeQueue, *fakeLauncher, *bytes.Buffer) {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	started := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	session := store.Session{
		ID: "session-1", Agent: "codex", NativeSessionID: "native-1",
		SourcePath: "/tmp/session.jsonl", SourceSize: 1, SourceMTime: started,
		SourceHash: "hash", WorkingDirectory: "/work/login",
		StartedAt: started, LastActiveAt: started.Add(time.Hour),
	}
	messages := []store.Message{
		{Sequence: 0, Timestamp: started, Role: "user", Text: "sensitive transcript phrase about login"},
		{Sequence: 1, Timestamp: started.Add(time.Hour), Role: "assistant", Text: "login retry fixed"},
	}
	if _, err := database.ImportSession(context.Background(), session, messages); err != nil {
		t.Fatal(err)
	}
	sequence := 1
	if err := database.ReplaceAnalysis(context.Background(), session.ID, store.Analysis{
		Title: "Login retry investigation", Summary: "Fixed login retry behavior.", Status: "current",
		Provider: "fake", Model: "test", PromptVersion: "v1", AnalyzedAt: started.Add(time.Hour),
		AnalyzedHash: "analysis", AnalyzedThroughSequence: &sequence, AnalyzedThroughAt: started.Add(time.Hour),
		Segments: []store.Segment{{Position: 0, StartSequence: 0, EndSequence: 1, Title: "Login", Summary: "Fixed retry", Detail: "Verified behavior"}},
	}); err != nil {
		t.Fatal(err)
	}
	scanner := &fakeScanner{}
	queue := &fakeQueue{}
	launcher := &fakeLauncher{}
	logs := &bytes.Buffer{}
	handler, err := NewHandler(Config{
		Token: "test-token", Store: database, Scanner: scanner, Queue: queue,
		Launcher: launcher, Logger: slog.New(slog.NewTextHandler(logs, nil)),
		AnalysisDefaults: analyze.Options{
			Provider: "codex-cli", Model: "default-model", PromptVersion: "v1",
			NormalizerVersion: "v1", LeafTargetChars: 12_000, RollupFanout: 8,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, database, scanner, queue, launcher, logs
}

func apiRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Host = "127.0.0.1:1234"
	return request
}

func jsonRequest(method, target, body string) *http.Request {
	request := apiRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1:1234")
	return request
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatalf("decode response: %v body=%s", err, response.Body.String())
	}
}

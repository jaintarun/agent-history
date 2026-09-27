package grok

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaintarun/agent-history/internal/source"
)

func TestReadNormalizesVisibleUpdates(t *testing.T) {
	adapter := New(t.TempDir())
	imported, err := adapter.Read(context.Background(), candidateForFixture(t, "basic"))
	if err != nil {
		t.Fatal(err)
	}
	if imported.Session.Agent != "grok" {
		t.Fatalf("agent = %q", imported.Session.Agent)
	}
	if imported.Session.NativeSessionID != "01a00000-0000-7000-8000-000000000001" {
		t.Fatalf("native session ID = %q", imported.Session.NativeSessionID)
	}
	if imported.Session.WorkingDirectory != "/Users/example/work/grok-project" {
		t.Fatalf("cwd = %q", imported.Session.WorkingDirectory)
	}
	if got := imported.Session.StartedAt.Format(time.RFC3339); got != "2026-09-01T09:59:00Z" {
		t.Fatalf("started at = %q", got)
	}
	if got := imported.Session.LastActiveAt.Format(time.RFC3339); got != "2026-09-01T10:01:00Z" {
		t.Fatalf("last active at = %q", got)
	}
	got, err := json.MarshalIndent(goldenMessages(imported.Messages), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "basic", "basic.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatalf("normalized messages differ\ngot:\n%s\nwant:\n%s", got, want)
	}
	serialized := string(got)
	for _, hidden := range []string{
		"private reasoning", "system instructions", "internal recap", "private hook", "hidden plan",
		"intermediate duplicate", "go test ./...", "tests pass",
	} {
		if strings.Contains(serialized, hidden) {
			t.Errorf("normalized messages contain hidden text %q", hidden)
		}
	}
}

func TestReadIgnoresIncompleteFinalUpdate(t *testing.T) {
	imported, err := New(t.TempDir()).Read(context.Background(), candidateForFixture(t, "partial"))
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(imported.Messages))
	}
}

func TestParseUpdatesDropsRewoundBranch(t *testing.T) {
	input := strings.Join([]string{
		`{"method":"session/update","timestamp":1788256800,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"keep prompt"},"_meta":{"promptIndex":0}}}}`,
		`{"method":"session/update","timestamp":1788256801,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"keep response"}}}}`,
		`{"method":"session/update","timestamp":1788256802,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"discarded prompt"},"_meta":{"promptIndex":1}}}}`,
		`{"method":"session/update","timestamp":1788256803,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"discarded response"}}}}`,
		`{"method":"_x.ai/session/update","timestamp":1788256804,"params":{"update":{"sessionUpdate":"rewind_marker","target_prompt_index":1}}}`,
		`{"method":"session/update","timestamp":1788256805,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"replacement prompt"},"_meta":{"promptIndex":1}}}}`,
		`{"method":"session/update","timestamp":1788256806,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"replacement response"}}}}`,
	}, "\n")

	messages, err := parseUpdates(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"keep prompt", "keep response", "replacement prompt", "replacement response"}
	if len(messages) != len(want) {
		t.Fatalf("messages = %#v, want texts %q", messages, want)
	}
	for i, message := range messages {
		if message.Text != want[i] || message.Sequence != i {
			t.Fatalf("message %d = %#v, want text %q and sequence %d", i, message, want[i], i)
		}
	}
}

func TestParseUpdatesExcludesInternalHiddenAndHostUpdates(t *testing.T) {
	input := strings.Join([]string{
		`{"method":"session/update","timestamp":1788256800,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"visible user"}}}}`,
		`{"method":"_x.ai/session/update","timestamp":1788256801,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"internal assistant"}}}}`,
		`{"method":"session/update","timestamp":1788256802,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hidden assistant"},"_meta":{"hideFromScrollback":true}}}}`,
		`{"method":"session/update","timestamp":1788256803,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"host user"},"_meta":{"hostTurn":true}}}}`,
		`{"method":"session/update","timestamp":1788256804,"params":{"update":{"sessionUpdate":"tool_call","toolCallId":"hidden-tool","rawInput":{"command":"private command"},"_meta":{"hideFromScrollback":true,"x.ai/tool":{"name":"bash"}}}}}`,
		`{"method":"session/update","timestamp":1788256805,"params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"hidden-tool","status":"completed","rawOutput":{"output_for_prompt":"private output"}}}}`,
		`{"method":"_x.ai/session/update","timestamp":1788256806,"params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"internal-tool","status":"completed","rawOutput":{"output_for_prompt":"internal output"}}}}`,
		`{"method":"session/update","timestamp":1788256807,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"visible assistant"}}}}`,
	}, "\n")

	messages, err := parseUpdates(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Text != "visible user" || messages[1].Text != "visible assistant" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestReadKeepsNonzeroSummaryTimestamps(t *testing.T) {
	dir := t.TempDir()
	copyFixture(t, "basic", dir)
	summary := `{
  "created_at": "2026-09-01T10:00:10Z",
  "last_active_at": "2026-09-01T10:00:20Z",
  "info": {
    "id": "01a00000-0000-7000-8000-000000000003",
    "cwd": "/Users/example/work/grok-project"
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(summary), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "updates.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := New(t.TempDir()).Read(context.Background(), source.Candidate{
		Path: path, Size: info.Size(), ModTime: info.ModTime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := imported.Session.StartedAt.Format(time.RFC3339); got != "2026-09-01T10:00:10Z" {
		t.Fatalf("started at = %q", got)
	}
	if got := imported.Session.LastActiveAt.Format(time.RFC3339); got != "2026-09-01T10:00:20Z" {
		t.Fatalf("last active at = %q", got)
	}
}

func TestParseUpdatesDropsToolCallsAndResults(t *testing.T) {
	input := strings.Join([]string{
		`{"method":"session/update","timestamp":1788256799,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"visible request"}}}}`,
		`{"method":"session/update","timestamp":1788256800,"params":{"update":{"sessionUpdate":"tool_call","toolCallId":"unfinished","rawInput":{"command":"do not retain"},"_meta":{"x.ai/tool":{"name":"bash"}}}}}`,
		`{"method":"session/update","timestamp":1788256801,"params":{"update":{"sessionUpdate":"tool_call","toolCallId":"finished","rawInput":{"command":"retain"},"_meta":{"x.ai/tool":{"name":"bash"}}}}}`,
		`{"method":"session/update","timestamp":1788256802,"params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"finished","status":"completed","rawOutput":{"output_for_prompt":"done"}}}}`,
		`{"method":"session/update","timestamp":1788256803,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"visible answer"}}}}`,
	}, "\n")

	messages, err := parseUpdates(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[0].Text != "visible request" ||
		messages[1].Role != "assistant" || messages[1].Text != "visible answer" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestParseUpdatesRewindKeepsOnlyCurrentConversation(t *testing.T) {
	input := strings.Join([]string{
		`{"method":"session/update","timestamp":1788256800,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"keep"},"_meta":{"promptIndex":0}}}}`,
		`{"method":"session/update","timestamp":1788256801,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"discard"},"_meta":{"promptIndex":1}}}}`,
		`{"method":"session/update","timestamp":1788256802,"params":{"update":{"sessionUpdate":"tool_call","toolCallId":"reused","rawInput":{"command":"discard"},"_meta":{"x.ai/tool":{"name":"bash"}}}}}`,
		`{"method":"session/update","timestamp":1788256803,"params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"reused","status":"completed","rawOutput":{"output_for_prompt":"discard"}}}}`,
		`{"method":"session/update","timestamp":1788256803,"params":{"update":{"sessionUpdate":"tool_call","toolCallId":"suppressed","rawInput":{"command":"hidden"},"_meta":{"hideFromScrollback":true,"x.ai/tool":{"name":"bash"}}}}}`,
		`{"method":"_x.ai/session/update","timestamp":1788256804,"params":{"update":{"sessionUpdate":"rewind_marker","target_prompt_index":1}}}`,
		`{"method":"session/update","timestamp":1788256805,"params":{"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"replacement"},"_meta":{"promptIndex":1}}}}`,
		`{"method":"session/update","timestamp":1788256806,"params":{"update":{"sessionUpdate":"tool_call","toolCallId":"reused","rawInput":{"command":"replacement"},"_meta":{"x.ai/tool":{"name":"bash"}}}}}`,
		`{"method":"session/update","timestamp":1788256807,"params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"reused","status":"completed","rawOutput":{"output_for_prompt":"replacement result"}}}}`,
		`{"method":"session/update","timestamp":1788256808,"params":{"update":{"sessionUpdate":"tool_call","toolCallId":"suppressed","rawInput":{"command":"visible after rewind"},"_meta":{"x.ai/tool":{"name":"bash"}}}}}`,
		`{"method":"session/update","timestamp":1788256809,"params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"suppressed","status":"completed","rawOutput":{"output_for_prompt":"visible suppressed result"}}}}`,
		`{"method":"session/update","timestamp":1788256810,"params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"replacement answer"}}}}`,
	}, "\n")

	messages, err := parseUpdates(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Text != "keep" || messages[1].Text != "replacement" ||
		messages[2].Text != "replacement answer" {
		t.Fatalf("messages after rewind = %#v", messages)
	}
}

func TestDiscoverFindsOnlyTopLevelSessionUpdates(t *testing.T) {
	home := t.TempDir()
	mainDir := filepath.Join(home, "sessions", "%2FUsers%2Fexample%2Fwork", "01a00000-0000-7000-8000-000000000001")
	copyFixture(t, "basic", mainDir)
	copyFixture(t, "basic", filepath.Join(mainDir, "subagents", "child"))

	candidates, err := New(home).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(mainDir, "updates.jsonl")
	if len(candidates) != 1 || candidates[0].Agent != "grok" ||
		candidates[0].NativeSessionID != "01a00000-0000-7000-8000-000000000001" ||
		candidates[0].Path != wantPath {
		t.Fatalf("candidates = %#v", candidates)
	}
}

func TestDefaultHomeUsesGrokHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "custom-grok")
	t.Setenv("GROK_HOME", home)
	if got := DefaultHome(); got != home {
		t.Fatalf("DefaultHome() = %q, want %q", got, home)
	}
}

func TestResumeSpec(t *testing.T) {
	imported, err := New(t.TempDir()).Read(context.Background(), candidateForFixture(t, "basic"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := New(t.TempDir()).ResumeSpec(imported.Session)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Executable != "grok" || len(spec.Args) != 2 || spec.Args[0] != "--resume" || spec.Args[1] != imported.Session.NativeSessionID {
		t.Fatalf("resume spec = %#v", spec)
	}
}

func FuzzParseUpdates(f *testing.F) {
	for _, name := range []string{"basic", "partial"} {
		seed, err := os.ReadFile(filepath.Join("testdata", name, "updates.jsonl"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		messages, err := parseUpdates(context.Background(), strings.NewReader(string(input)))
		if err != nil {
			return
		}
		for i, message := range messages {
			if message.Sequence != i {
				t.Fatalf("message sequence = %d at position %d", message.Sequence, i)
			}
		}
	})
}

type goldenMessage struct {
	Sequence  int    `json:"sequence"`
	Timestamp string `json:"timestamp"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	ToolName  string `json:"tool_name,omitempty"`
}

func goldenMessages(messages []source.Message) []goldenMessage {
	result := make([]goldenMessage, 0, len(messages))
	for _, message := range messages {
		result = append(result, goldenMessage{
			Sequence: message.Sequence, Timestamp: message.Timestamp.Format(time.RFC3339Nano),
			Role: message.Role, Text: message.Text, ToolName: message.ToolName,
		})
	}
	return result
}

func candidateForFixture(t *testing.T, name string) source.Candidate {
	t.Helper()
	path := filepath.Join("testdata", name, "updates.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return source.Candidate{Path: path, Size: info.Size(), ModTime: info.ModTime()}
}

func copyFixture(t *testing.T, name, destination string) {
	t.Helper()
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"summary.json", "updates.jsonl"} {
		content, err := os.ReadFile(filepath.Join("testdata", name, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, file), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

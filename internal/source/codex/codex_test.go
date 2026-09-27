package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jaintarun/agent-history/internal/source"
)

func TestReadNormalizesVisibleRecords(t *testing.T) {
	adapter := New(t.TempDir())
	candidate := candidateForFixture(t, "basic.jsonl")

	imported, err := adapter.Read(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Session.NativeSessionID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("native session ID = %q", imported.Session.NativeSessionID)
	}
	if imported.Session.WorkingDirectory != "/Users/example/work/login-service" {
		t.Fatalf("cwd = %q", imported.Session.WorkingDirectory)
	}
	if imported.Session.StartedAt.Format(timestampLayout) != "2026-07-01T10:00:00Z" ||
		imported.Session.LastActiveAt.Format(timestampLayout) != "2026-07-01T10:04:00Z" {
		t.Fatalf("derived times = %s to %s", imported.Session.StartedAt, imported.Session.LastActiveAt)
	}

	got, err := json.MarshalIndent(goldenMessages(imported.Messages), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "basic.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Fatalf("normalized messages differ\ngot:\n%s\nwant:\n%s", got, want)
	}
	serialized := string(got)
	for _, message := range imported.Messages {
		if message.Role == "tool" {
			t.Fatalf("tool message was retained: %#v", message)
		}
	}
	for _, hidden := range []string{"chain of thought", "developer instructions", "injected instructions", "duplicate user", "duplicate assistant"} {
		if strings.Contains(serialized, hidden) {
			t.Errorf("normalized messages contain hidden or duplicate text %q", hidden)
		}
	}
}

func TestReadCurrentResponseItemConversation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.jsonl")
	content := strings.Join([]string{
		`{"timestamp":"2026-07-01T10:00:00Z","type":"session_meta","payload":{"id":"current","cwd":"/tmp/project"}}`,
		`{"timestamp":"2026-07-01T10:01:00Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"hidden policy"}]}}`,
		`{"timestamp":"2026-07-01T10:02:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Build the app"}]}}`,
		`{"timestamp":"2026-07-01T10:03:00Z","type":"response_item","payload":{"type":"reasoning","content":[{"type":"reasoning_text","text":"hidden reasoning"}]}}`,
		`{"timestamp":"2026-07-01T10:04:00Z","type":"response_item","payload":{"type":"function_call_output","output":"hidden tool output"}}`,
		`{"timestamp":"2026-07-01T10:05:00Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Working on it"}]}}`,
		`{"timestamp":"2026-07-01T10:06:00Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"The app is running"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := New(t.TempDir()).Read(context.Background(), source.Candidate{Path: path, Size: info.Size(), ModTime: info.ModTime()})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, message := range imported.Messages {
		got = append(got, message.Role+":"+message.Text)
	}
	want := []string{"user:Build the app", "assistant:Working on it", "assistant:The app is running"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %#v, want %#v", got, want)
	}
}

func TestResumeSpec(t *testing.T) {
	imported, err := New(t.TempDir()).Read(context.Background(), candidateForFixture(t, "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := New(t.TempDir()).ResumeSpec(imported.Session)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Executable != "codex" || len(spec.Args) != 2 || spec.Args[0] != "resume" || spec.Args[1] != imported.Session.NativeSessionID {
		t.Fatalf("resume spec = %#v", spec)
	}
}

func TestReadIgnoresIncompleteFinalRecord(t *testing.T) {
	adapter := New(t.TempDir())
	imported, err := adapter.Read(context.Background(), candidateForFixture(t, "partial.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(imported.Messages))
	}
}

func TestParseKeepsFirstSessionMetadata(t *testing.T) {
	content := "" +
		`{"timestamp":"2026-07-01T10:00:00Z","type":"session_meta","payload":{"id":"first-session","cwd":"/tmp/first"}}` + "\n" +
		`{"timestamp":"2026-07-01T10:01:00Z","type":"session_meta","payload":{"id":"later-session","cwd":"/tmp/later"}}` + "\n" +
		`{"timestamp":"2026-07-01T10:02:00Z","type":"event_msg","payload":{"type":"user_message","message":"Visible"}}` + "\n"
	parsed, err := parseRollout(context.Background(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.meta.ID != "first-session" || parsed.meta.CWD != "/tmp/first" {
		t.Fatalf("metadata = %#v", parsed.meta)
	}
}

func TestDiscoverDeduplicatesAndPrefersActiveSession(t *testing.T) {
	home := t.TempDir()
	active := filepath.Join(home, "sessions", "2026", "07", "rollout-active.jsonl")
	archived := filepath.Join(home, "archived_sessions", "rollout-archived.jsonl")
	copyFixture(t, "basic.jsonl", active)
	copyFixture(t, "basic.jsonl", archived)

	candidates, err := New(home).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	if candidates[0].Path != active || candidates[0].Archived {
		t.Fatalf("candidate = %#v, want active path", candidates[0])
	}
}

func TestDiscoverExcludesSubagentSessions(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		name   string
		source string
		want   bool
	}{
		{name: "interactive", source: `"cli"`},
		{name: "spawned", source: `{"subagent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}`, want: true},
		{name: "review", source: `{"subagent":"review"}`, want: true},
	}
	for _, tc := range cases {
		path := filepath.Join(home, "sessions", tc.name+".jsonl")
		content := `{"timestamp":"2026-07-01T10:00:00Z","type":"session_meta","payload":{"id":"` + tc.name + `","cwd":"/tmp/project","source":` + tc.source + `}}` + "\n"
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := New(home).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != len(cases) {
		t.Fatalf("candidate count = %d, want %d", len(candidates), len(cases))
	}
	for _, candidate := range candidates {
		for _, tc := range cases {
			if candidate.NativeSessionID == tc.name && candidate.Excluded != tc.want {
				t.Errorf("%s excluded = %t, want %t", tc.name, candidate.Excluded, tc.want)
			}
		}
	}
}

func TestReadIgnoresLargeToolOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	large := strings.Repeat("x", 64<<10+100)
	content := `{"timestamp":"2026-07-01T09:59:00Z","type":"session_meta","payload":{"id":"33333333-3333-4333-8333-333333333333","cwd":"/tmp/project"}}` + "\n" +
		`{"timestamp":"2026-07-01T10:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"Run it."}}` + "\n" +
		`{"timestamp":"2026-07-01T10:01:00Z","type":"response_item","payload":{"type":"function_call_output","call_id":"unknown","output":` + mustJSON(t, large) + `}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := New(t.TempDir()).Read(context.Background(), source.Candidate{Path: path, Size: info.Size(), ModTime: info.ModTime()})
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Messages) != 1 || imported.Messages[0].Text != "Run it." {
		t.Fatalf("messages = %#v, want only the user request", imported.Messages)
	}
}

func FuzzParseRollout(f *testing.F) {
	for _, name := range []string{"basic.jsonl", "partial.jsonl"} {
		seed, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(seed)
	}
	f.Add([]byte("not json\n"))
	f.Fuzz(func(t *testing.T, input []byte) {
		parsed, err := parseRollout(context.Background(), strings.NewReader(string(input)))
		if err != nil {
			return
		}
		for i, message := range parsed.messages {
			if message.Sequence != i {
				t.Fatalf("message sequence = %d at position %d", message.Sequence, i)
			}
			switch message.Role {
			case "user", "assistant":
			default:
				t.Fatalf("unexpected normalized role %q", message.Role)
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
			Sequence: message.Sequence, Timestamp: message.Timestamp.Format(timestampLayout),
			Role: message.Role, Text: message.Text, ToolName: message.ToolName,
		})
	}
	return result
}

func candidateForFixture(t *testing.T, name string) source.Candidate {
	t.Helper()
	path := filepath.Join("testdata", name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return source.Candidate{Path: path, Size: info.Size(), ModTime: info.ModTime()}
}

func copyFixture(t *testing.T, name, destination string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", name))
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

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

package claude

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

func TestReadNormalizesVisibleRecords(t *testing.T) {
	adapter := New(t.TempDir())
	imported, err := adapter.Read(context.Background(), candidateForFixture(t, "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if imported.Session.NativeSessionID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("native session ID = %q", imported.Session.NativeSessionID)
	}
	if imported.Session.WorkingDirectory != "/Users/example/work/payments" {
		t.Fatalf("cwd = %q", imported.Session.WorkingDirectory)
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
	for _, hidden := range []string{"private reasoning", "system instructions", "injected instruction", "hidden subagent", "hidden reminder", "command-name"} {
		if strings.Contains(serialized, hidden) {
			t.Errorf("normalized messages contain hidden text %q", hidden)
		}
	}
}

func TestReadIgnoresIncompleteFinalRecord(t *testing.T) {
	imported, err := New(t.TempDir()).Read(context.Background(), candidateForFixture(t, "partial.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(imported.Messages))
	}
}

func TestDiscoverExcludesNestedSubagents(t *testing.T) {
	home := t.TempDir()
	mainPath := filepath.Join(home, "projects", "-Users-example-work", "55555555-5555-4555-8555-555555555555.jsonl")
	subagentPath := filepath.Join(home, "projects", "-Users-example-work", "55555555-5555-4555-8555-555555555555", "subagents", "agent-a.jsonl")
	copyFixture(t, "basic.jsonl", mainPath)
	copyFixture(t, "basic.jsonl", subagentPath)

	candidates, err := New(home).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Path != mainPath {
		t.Fatalf("candidates = %#v, want main session only", candidates)
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
	if spec.Executable != "claude" || len(spec.Args) != 2 || spec.Args[0] != "--resume" || spec.Args[1] != imported.Session.NativeSessionID {
		t.Fatalf("resume spec = %#v", spec)
	}
}

func FuzzParseTranscript(f *testing.F) {
	for _, name := range []string{"basic.jsonl", "partial.jsonl"} {
		seed, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		parsed, err := parseTranscript(context.Background(), strings.NewReader(string(input)))
		if err != nil {
			return
		}
		for i, message := range parsed.messages {
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

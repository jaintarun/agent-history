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
		"intermediate duplicate",
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

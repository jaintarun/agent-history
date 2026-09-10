package cmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadHookMappingsReadsClaudeCodexAndGrok(t *testing.T) {
	home := t.TempDir()
	writeHookFixture(t, home, "claude", `{
  "sessions": {
    "one": {"sessionId":"claude-native","workspaceId":"w1","surfaceId":"s1","agentLifecycle":"needsInput","updatedAt":100.5}
  }
}`)
	writeHookFixture(t, home, "codex", `{
  "sessions": {
    "two": {"sessionId":"codex-native","workspaceId":"w2","surfaceId":"s2","agentLifecycle":"idle","updatedAt":200}
  }
}`)
	writeHookFixture(t, home, "grok", `{
  "sessions": {
    "three": {"sessionId":"grok-native","workspaceId":"w3","surfaceId":"s3","agentLifecycle":"running","updatedAt":300}
  }
}`)

	mappings, diagnostics := LoadHookMappings(home)
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	if len(mappings) != 3 || mappings[0].Agent != "claude" || mappings[1].Agent != "codex" || mappings[2].Agent != "grok" {
		t.Fatalf("mappings = %#v", mappings)
	}
	if mappings[0].Lifecycle != "needsInput" || mappings[1].NativeSessionID != "codex-native" ||
		mappings[2].Lifecycle != "running" || mappings[2].NativeSessionID != "grok-native" {
		t.Fatalf("mappings = %#v", mappings)
	}
}

func TestLoadHookMappingsKeepsNewestValidMappingAndIsolatesMalformedAgent(t *testing.T) {
	home := t.TempDir()
	writeHookFixture(t, home, "claude", `{
  "sessions": {
    "old": {"sessionId":"same","workspaceId":"old-workspace","surfaceId":"old-surface","agentLifecycle":"busy","updatedAt":10},
    "new": {"sessionId":"same","workspaceId":"new-workspace","surfaceId":"new-surface","agentLifecycle":"running","updatedAt":20},
    "invalid": {"sessionId":"missing-target","workspaceId":"","surfaceId":"","updatedAt":30}
  }
}`)
	writeHookFixture(t, home, "codex", `{broken`)

	mappings, diagnostics := LoadHookMappings(home)
	if len(mappings) != 1 || mappings[0].WorkspaceID != "new-workspace" || mappings[0].Lifecycle != "running" {
		t.Fatalf("mappings = %#v", mappings)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0].Error(), "codex") {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
}

func TestLoadHookMappingsKeepsExplicitlyActiveSessionPerAgentSurface(t *testing.T) {
	home := t.TempDir()
	writeHookFixture(t, home, "grok", `{
  "activeSessionsBySurface": {
    "s1": {"sessionId":"current","updatedAt":200}
  },
  "sessions": {
    "current": {"sessionId":"current","workspaceId":"w1","surfaceId":"s1","agentLifecycle":"idle","updatedAt":200},
    "stale": {"sessionId":"stale","workspaceId":"w1","surfaceId":"s1","agentLifecycle":"running","updatedAt":300}
  }
}`)

	mappings, diagnostics := LoadHookMappings(home)
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	mappings = selectCurrentHookMappings(mappings, map[string]string{
		"grok\x00current": "current", "grok\x00stale": "stale",
	})
	if len(mappings) != 1 || mappings[0].NativeSessionID != "current" || mappings[0].Lifecycle != "idle" {
		t.Fatalf("mappings = %#v", mappings)
	}
}

func TestLoadHookMappingsPrefersLiveProcessWithoutActiveMarker(t *testing.T) {
	home := t.TempDir()
	writeHookFixture(t, home, "grok", fmt.Sprintf(`{
  "sessions": {
    "current": {"sessionId":"current","workspaceId":"w1","surfaceId":"s1","agentLifecycle":"idle","pid":%d,"updatedAt":200},
    "stale": {"sessionId":"stale","workspaceId":"w1","surfaceId":"s1","agentLifecycle":"running","pid":99999999,"updatedAt":300}
  }
}`, os.Getpid()))

	mappings, diagnostics := LoadHookMappings(home)
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	mappings = selectCurrentHookMappings(mappings, map[string]string{
		"grok\x00current": "current", "grok\x00stale": "stale",
	})
	if len(mappings) != 1 || mappings[0].NativeSessionID != "current" || mappings[0].Lifecycle != "idle" {
		t.Fatalf("mappings = %#v", mappings)
	}
}

func writeHookFixture(t *testing.T, home, agent, content string) {
	t.Helper()
	directory := filepath.Join(home, ".cmuxterm")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, agent+"-hook-sessions.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

package cmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// HookMapping is the documented native-session-to-cmux identity mapping.
type HookMapping struct {
	Agent           string
	NativeSessionID string
	WorkspaceID     string
	SurfaceID       string
	Lifecycle       string
	UpdatedAt       time.Time
}

type hookFile struct {
	Sessions map[string]struct {
		SessionID      string  `json:"sessionId"`
		WorkspaceID    string  `json:"workspaceId"`
		SurfaceID      string  `json:"surfaceId"`
		AgentLifecycle string  `json:"agentLifecycle"`
		UpdatedAt      float64 `json:"updatedAt"`
	} `json:"sessions"`
}

// LoadHookMappings loads independent Claude and Codex hook stores. Missing
// files are normal; malformed files are isolated as per-agent diagnostics.
func LoadHookMappings(home string) ([]HookMapping, []error) {
	var diagnostics []error
	newest := make(map[string]HookMapping)
	for _, agent := range []string{"claude", "codex"} {
		path := filepath.Join(home, ".cmuxterm", agent+"-hook-sessions.json")
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			diagnostics = append(diagnostics, fmt.Errorf("read %s cmux hooks: %w", agent, err))
			continue
		}
		var file hookFile
		if err := json.Unmarshal(content, &file); err != nil {
			diagnostics = append(diagnostics, fmt.Errorf("decode %s cmux hooks: %w", agent, err))
			continue
		}
		for _, session := range file.Sessions {
			if session.SessionID == "" || session.WorkspaceID == "" || session.SurfaceID == "" {
				continue
			}
			mapping := HookMapping{
				Agent: agent, NativeSessionID: session.SessionID,
				WorkspaceID: session.WorkspaceID, SurfaceID: session.SurfaceID,
				Lifecycle: normalizeLifecycle(session.AgentLifecycle),
				UpdatedAt: time.Unix(0, int64(session.UpdatedAt*float64(time.Second))).UTC(),
			}
			key := agent + "\x00" + session.SessionID
			if previous, exists := newest[key]; !exists || mapping.UpdatedAt.After(previous.UpdatedAt) ||
				(mapping.UpdatedAt.Equal(previous.UpdatedAt) && mapping.WorkspaceID+mapping.SurfaceID > previous.WorkspaceID+previous.SurfaceID) {
				newest[key] = mapping
			}
		}
	}
	mappings := make([]HookMapping, 0, len(newest))
	for _, mapping := range newest {
		mappings = append(mappings, mapping)
	}
	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].Agent != mappings[j].Agent {
			return mappings[i].Agent < mappings[j].Agent
		}
		return mappings[i].NativeSessionID < mappings[j].NativeSessionID
	})
	return mappings, diagnostics
}

func normalizeLifecycle(value string) string {
	switch value {
	case "running", "idle", "needsInput":
		return value
	default:
		return "unknown"
	}
}

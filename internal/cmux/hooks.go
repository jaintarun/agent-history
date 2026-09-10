package cmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
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
	pid             int
	active          bool
}

type hookFile struct {
	ActiveSessionsBySurface map[string]struct {
		SessionID string `json:"sessionId"`
	} `json:"activeSessionsBySurface"`
	Sessions map[string]struct {
		SessionID      string  `json:"sessionId"`
		WorkspaceID    string  `json:"workspaceId"`
		SurfaceID      string  `json:"surfaceId"`
		AgentLifecycle string  `json:"agentLifecycle"`
		PID            int     `json:"pid"`
		UpdatedAt      float64 `json:"updatedAt"`
	} `json:"sessions"`
}

// LoadHookMappings loads independent Claude, Codex, and Grok hook stores. Missing
// files are normal; malformed files are isolated as per-agent diagnostics.
func LoadHookMappings(home string) ([]HookMapping, []error) {
	var diagnostics []error
	newest := make(map[string]HookMapping)
	for _, agent := range []string{"claude", "codex", "grok"} {
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
				pid:       session.PID,
			}
			mapping.active = file.ActiveSessionsBySurface[session.SurfaceID].SessionID == session.SessionID
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
	sortHookMappings(mappings)
	return mappings, diagnostics
}

func selectCurrentHookMappings(mappings []HookMapping, identities map[string]string) []HookMapping {
	current := make(map[string]HookMapping)
	for _, mapping := range mappings {
		if _, exists := identities[mapping.Agent+"\x00"+mapping.NativeSessionID]; !exists {
			continue
		}
		key := mapping.Agent + "\x00" + mapping.WorkspaceID + "\x00" + mapping.SurfaceID
		if previous, exists := current[key]; !exists || preferHookMapping(mapping, previous) {
			current[key] = mapping
		}
	}
	selected := make([]HookMapping, 0, len(current))
	for _, mapping := range current {
		selected = append(selected, mapping)
	}
	sortHookMappings(selected)
	return selected
}

func sortHookMappings(mappings []HookMapping) {
	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].Agent != mappings[j].Agent {
			return mappings[i].Agent < mappings[j].Agent
		}
		return mappings[i].NativeSessionID < mappings[j].NativeSessionID
	})
}

func preferHookMapping(candidate, current HookMapping) bool {
	if candidate.active != current.active {
		return candidate.active
	}
	candidateAlive, currentAlive := processAlive(candidate.pid), processAlive(current.pid)
	if candidateAlive != currentAlive {
		return candidateAlive
	}
	if !candidate.UpdatedAt.Equal(current.UpdatedAt) {
		return candidate.UpdatedAt.After(current.UpdatedAt)
	}
	return candidate.NativeSessionID > current.NativeSessionID
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func normalizeLifecycle(value string) string {
	switch value {
	case "running", "idle", "needsInput":
		return value
	default:
		return "unknown"
	}
}

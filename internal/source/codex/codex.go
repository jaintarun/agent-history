// Package codex discovers and normalizes Codex rollout transcripts.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/store"
)

const (
	timestampLayout  = time.RFC3339Nano
	maxToolText      = 64 << 10
	truncationMarker = "\n[tool output truncated]"
)

var errNoMetadata = errors.New("codex: session metadata not found")

// Adapter reads Codex history under one CODEX_HOME directory.
type Adapter struct {
	home string
}

// New constructs a Codex source rooted at home.
func New(home string) *Adapter {
	return &Adapter{home: home}
}

// DefaultHome returns CODEX_HOME or the conventional ~/.codex path.
func DefaultHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

// Name returns the normalized source-agent name.
func (a *Adapter) Name() string { return "codex" }

// Discover finds active and archived JSONL rollouts and deduplicates them by
// native session ID, preferring an active path.
func (a *Adapter) Discover(ctx context.Context) ([]source.Candidate, error) {
	type root struct {
		path     string
		archived bool
	}
	roots := []root{
		{path: filepath.Join(a.home, "sessions")},
		{path: filepath.Join(a.home, "archived_sessions"), archived: true},
	}
	byID := make(map[string]source.Candidate)
	for _, root := range roots {
		err := filepath.WalkDir(root.path, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, os.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > source.MaxTranscriptBytes {
				return fmt.Errorf("transcript %q size %d exceeds %d bytes", path, info.Size(), source.MaxTranscriptBytes)
			}
			meta, err := readMetadata(path)
			if errors.Is(err, errNoMetadata) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read metadata %q: %w", path, err)
			}
			candidate := source.Candidate{
				Agent: "codex", NativeSessionID: meta.ID, Path: path,
				Size: info.Size(), ModTime: info.ModTime().UTC(), Archived: root.archived,
			}
			current, exists := byID[meta.ID]
			if !exists || (current.Archived && !candidate.Archived) ||
				(current.Archived == candidate.Archived && candidate.ModTime.After(current.ModTime)) {
				byID[meta.ID] = candidate
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("discover Codex sessions: %w", err)
		}
	}
	candidates := make([]source.Candidate, 0, len(byID))
	for _, candidate := range byID {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, nil
}

// Read parses a rollout completely and retains only visible conversation and
// bounded tool activity.
func (a *Adapter) Read(ctx context.Context, candidate source.Candidate) (source.ImportedSession, error) {
	file, err := os.Open(candidate.Path)
	if err != nil {
		return source.ImportedSession{}, fmt.Errorf("open Codex transcript: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return source.ImportedSession{}, fmt.Errorf("stat Codex transcript: %w", err)
	}
	if info.Size() > source.MaxTranscriptBytes {
		return source.ImportedSession{}, fmt.Errorf("Codex transcript size %d exceeds %d bytes", info.Size(), source.MaxTranscriptBytes)
	}
	hasher := sha256.New()
	parsed, err := parseRollout(ctx, io.TeeReader(file, hasher))
	if err != nil {
		return source.ImportedSession{}, fmt.Errorf("parse Codex transcript %q: %w", candidate.Path, err)
	}
	if parsed.meta.ID == "" {
		return source.ImportedSession{}, errNoMetadata
	}
	if candidate.Size == 0 || candidate.ModTime.IsZero() {
		candidate.Size = info.Size()
		candidate.ModTime = info.ModTime().UTC()
	}
	startedAt, lastActiveAt := parsed.meta.Timestamp, parsed.meta.Timestamp
	if len(parsed.messages) != 0 {
		startedAt = parsed.messages[0].Timestamp
		lastActiveAt = parsed.messages[len(parsed.messages)-1].Timestamp
	}
	if startedAt.IsZero() {
		startedAt = candidate.ModTime.UTC()
		lastActiveAt = startedAt
	}
	sessionID := source.StableID("codex", parsed.meta.ID)
	for i := range parsed.messages {
		parsed.messages[i].SessionID = sessionID
	}
	return source.ImportedSession{
		Session: store.Session{
			ID: sessionID, Agent: "codex", NativeSessionID: parsed.meta.ID,
			SourcePath: candidate.Path, SourceSize: candidate.Size,
			SourceMTime: candidate.ModTime.UTC(), SourceHash: hashString(hasher),
			WorkingDirectory: parsed.meta.CWD, StartedAt: startedAt.UTC(),
			LastActiveAt: lastActiveAt.UTC(), AnalysisStatus: "none",
		},
		Messages: parsed.messages,
	}, nil
}

// ResumeSpec returns the structured Codex resume invocation.
func (a *Adapter) ResumeSpec(session store.Session) (source.ResumeSpec, error) {
	if session.Agent != "codex" || session.NativeSessionID == "" {
		return source.ResumeSpec{}, errors.New("codex: invalid session")
	}
	return source.ResumeSpec{
		Agent: "codex", SessionID: session.NativeSessionID, CWD: session.WorkingDirectory,
		Executable: "codex", Args: []string{"resume", session.NativeSessionID},
	}, nil
}

type metadata struct {
	ID        string
	CWD       string
	Timestamp time.Time
}

type parsedRollout struct {
	meta     metadata
	messages []store.Message
}

type envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func parseRollout(ctx context.Context, input io.Reader) (parsedRollout, error) {
	reader := bufio.NewReader(input)
	var parsed parsedRollout
	toolNames := make(map[string]string)
	lineNumber := 0
	for {
		line, readErr := source.ReadJSONLRecord(reader)
		lineNumber++
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			var record envelope
			if err := json.Unmarshal([]byte(trimmed), &record); err != nil {
				if errors.Is(readErr, io.EOF) || readerAtEOF(reader) {
					break
				}
				return parsedRollout{}, fmt.Errorf("decode line %d: %w", lineNumber, err)
			}
			if err := consumeRecord(&parsed, toolNames, record); err != nil {
				return parsedRollout{}, fmt.Errorf("line %d: %w", lineNumber, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return parsedRollout{}, err
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return parsedRollout{}, readErr
		}
	}
	return parsed, nil
}

func readerAtEOF(reader *bufio.Reader) bool {
	_, err := reader.Peek(1)
	return errors.Is(err, io.EOF)
}

func consumeRecord(parsed *parsedRollout, toolNames map[string]string, record envelope) error {
	timestamp, err := parseTimestamp(record.Timestamp)
	if err != nil {
		return err
	}
	switch record.Type {
	case "session_meta":
		if parsed.meta.ID != "" {
			return nil
		}
		var payload struct {
			ID        string `json:"id"`
			CWD       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return err
		}
		metaTimestamp := timestamp
		if payload.Timestamp != "" {
			metaTimestamp, err = parseTimestamp(payload.Timestamp)
			if err != nil {
				return err
			}
		}
		parsed.meta = metadata{ID: payload.ID, CWD: payload.CWD, Timestamp: metaTimestamp}
	case "event_msg":
		var payload struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return err
		}
		role := ""
		switch payload.Type {
		case "user_message":
			role = "user"
		case "agent_message":
			role = "assistant"
		}
		if role != "" && strings.TrimSpace(payload.Message) != "" {
			appendMessage(parsed, timestamp, role, payload.Message, "")
		}
	case "response_item":
		return consumeResponseItem(parsed, toolNames, timestamp, record.Payload)
	}
	return nil
}

func consumeResponseItem(parsed *parsedRollout, toolNames map[string]string, timestamp time.Time, raw json.RawMessage) error {
	var payload struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Output    json.RawMessage `json:"output"`
		Action    json.RawMessage `json:"action"`
		Tools     json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	switch payload.Type {
	case "function_call", "custom_tool_call":
		toolNames[payload.CallID] = payload.Name
		arguments := payload.Arguments
		if payload.Type == "custom_tool_call" {
			arguments = payload.Input
		}
		text := strings.TrimSpace(payload.Name + " " + rawText(arguments))
		appendMessage(parsed, timestamp, "tool", boundToolText(text), payload.Name)
	case "function_call_output", "custom_tool_call_output":
		appendMessage(parsed, timestamp, "tool", boundToolText(rawText(payload.Output)), toolNames[payload.CallID])
	case "tool_search_call":
		toolNames[payload.CallID] = "tool_search"
		appendMessage(parsed, timestamp, "tool", boundToolText("tool_search "+rawText(payload.Arguments)), "tool_search")
	case "tool_search_output":
		appendMessage(parsed, timestamp, "tool", boundToolText(rawText(payload.Tools)), toolNames[payload.CallID])
	case "web_search_call":
		appendMessage(parsed, timestamp, "tool", boundToolText("web_search "+rawText(payload.Action)), "web_search")
	}
	return nil
}

func appendMessage(parsed *parsedRollout, timestamp time.Time, role, text, toolName string) {
	parsed.messages = append(parsed.messages, store.Message{
		Sequence: len(parsed.messages), Timestamp: timestamp.UTC(), Role: role,
		Text: text, ToolName: toolName,
	})
}

func readMetadata(path string) (metadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return metadata{}, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := source.ReadJSONLRecord(reader)
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			var record envelope
			if err := json.Unmarshal([]byte(trimmed), &record); err != nil {
				if errors.Is(readErr, io.EOF) {
					return metadata{}, errNoMetadata
				}
				return metadata{}, fmt.Errorf("decode line %d: %w", lineNumber, err)
			}
			if record.Type == "session_meta" {
				parsed := parsedRollout{}
				if err := consumeRecord(&parsed, map[string]string{}, record); err != nil {
					return metadata{}, err
				}
				if parsed.meta.ID == "" {
					return metadata{}, errNoMetadata
				}
				return parsed.meta, nil
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return metadata{}, errNoMetadata
			}
			return metadata{}, readErr
		}
	}
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return compact.String()
	}
	return string(raw)
}

func boundToolText(text string) string {
	if len(text) <= maxToolText {
		return text
	}
	end := maxToolText
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + truncationMarker
}

func parseTimestamp(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(timestampLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	return parsed.UTC(), nil
}

func hashString(hasher hash.Hash) string {
	return hex.EncodeToString(hasher.Sum(nil))
}

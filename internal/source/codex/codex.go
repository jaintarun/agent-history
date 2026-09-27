// Package codex discovers and normalizes Codex rollout transcripts.
package codex

import (
	"bufio"
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

	"github.com/jaintarun/agent-history/internal/source"
	"github.com/jaintarun/agent-history/internal/store"
)

const (
	timestampLayout               = time.RFC3339Nano
	conversationNormalizerVersion = "codex-conversation-v1"
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
				Excluded:          meta.IsSubagent,
				NormalizerVersion: conversationNormalizerVersion,
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

// Read parses a rollout completely and retains only visible conversation.
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
			SourceMTime: candidate.ModTime.UTC(), SourceHash: conversationNormalizerVersion + ":" + hashString(hasher),
			WorkingDirectory: parsed.meta.CWD, StartedAt: startedAt.UTC(),
			LastActiveAt: lastActiveAt.UTC(), AnalysisStatus: "none",
		},
		Messages: parsed.messages, NormalizerVersion: conversationNormalizerVersion,
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
	ID         string
	CWD        string
	Timestamp  time.Time
	IsSubagent bool
}

type parsedRollout struct {
	meta             metadata
	messages         []store.Message
	eventMessages    []store.Message
	responseMessages []store.Message
}

type envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func parseRollout(ctx context.Context, input io.Reader) (parsedRollout, error) {
	reader := bufio.NewReader(input)
	var parsed parsedRollout
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
			if err := consumeRecord(&parsed, record); err != nil {
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
	for _, role := range []string{"user", "assistant"} {
		selected := parsed.responseMessages
		for _, message := range parsed.eventMessages {
			if message.Role == role {
				selected = parsed.eventMessages
				break
			}
		}
		for _, message := range selected {
			if message.Role == role {
				parsed.messages = append(parsed.messages, message)
			}
		}
	}
	sort.SliceStable(parsed.messages, func(i, j int) bool {
		return parsed.messages[i].Timestamp.Before(parsed.messages[j].Timestamp)
	})
	for i := range parsed.messages {
		parsed.messages[i].Sequence = i
	}
	return parsed, nil
}

func readerAtEOF(reader *bufio.Reader) bool {
	_, err := reader.Peek(1)
	return errors.Is(err, io.EOF)
}

func consumeRecord(parsed *parsedRollout, record envelope) error {
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
			ID        string          `json:"id"`
			CWD       string          `json:"cwd"`
			Timestamp string          `json:"timestamp"`
			Source    json.RawMessage `json:"source"`
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
		isSubagent := false
		if len(payload.Source) > 0 && payload.Source[0] == '{' {
			var sourceFields map[string]json.RawMessage
			if err := json.Unmarshal(payload.Source, &sourceFields); err != nil {
				return err
			}
			_, isSubagent = sourceFields["subagent"]
		}
		parsed.meta = metadata{ID: payload.ID, CWD: payload.CWD, Timestamp: metaTimestamp, IsSubagent: isSubagent}
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
			parsed.eventMessages = append(parsed.eventMessages, store.Message{Timestamp: timestamp, Role: role, Text: payload.Message})
		}
	case "response_item":
		return consumeResponseItem(parsed, timestamp, record.Payload)
	}
	return nil
}

func consumeResponseItem(parsed *parsedRollout, timestamp time.Time, raw json.RawMessage) error {
	var payload struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Type != "message" || (payload.Role != "user" && payload.Role != "assistant") {
		return nil
	}
	contentType := "input_text"
	if payload.Role == "assistant" {
		contentType = "output_text"
	}
	var texts []string
	for _, block := range payload.Content {
		if block.Type == contentType && strings.TrimSpace(block.Text) != "" {
			texts = append(texts, block.Text)
		}
	}
	if len(texts) != 0 {
		parsed.responseMessages = append(parsed.responseMessages, store.Message{
			Timestamp: timestamp, Role: payload.Role, Text: strings.Join(texts, "\n"),
		})
	}
	return nil
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
				if err := consumeRecord(&parsed, record); err != nil {
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

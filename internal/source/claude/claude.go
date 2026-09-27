// Package claude discovers and normalizes Claude Code project transcripts.
package claude

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

const conversationNormalizerVersion = "claude-conversation-v1"

var errNoMetadata = errors.New("claude: session metadata not found")

// Adapter reads Claude Code history under one CLAUDE_CONFIG_DIR directory.
type Adapter struct {
	home string
}

// New constructs a Claude Code source rooted at home.
func New(home string) *Adapter { return &Adapter{home: home} }

// DefaultHome returns CLAUDE_CONFIG_DIR or the conventional ~/.claude path.
func DefaultHome() string {
	if home := os.Getenv("CLAUDE_CONFIG_DIR"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

// Name returns the normalized source-agent name.
func (a *Adapter) Name() string { return "claude" }

// Discover finds main project transcripts and excludes nested subagent logs.
func (a *Adapter) Discover(ctx context.Context) ([]source.Candidate, error) {
	root := filepath.Join(a.home, "projects")
	byID := make(map[string]source.Candidate)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "subagents" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
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
			Agent: "claude", NativeSessionID: meta.ID, Path: path,
			Size: info.Size(), ModTime: info.ModTime().UTC(),
			NormalizerVersion: conversationNormalizerVersion,
		}
		current, exists := byID[meta.ID]
		if !exists || candidate.ModTime.After(current.ModTime) {
			byID[meta.ID] = candidate
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("discover Claude sessions: %w", err)
	}
	candidates := make([]source.Candidate, 0, len(byID))
	for _, candidate := range byID {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, nil
}

// Read parses a Claude transcript completely and retains only visible main conversation.
func (a *Adapter) Read(ctx context.Context, candidate source.Candidate) (source.ImportedSession, error) {
	file, err := os.Open(candidate.Path)
	if err != nil {
		return source.ImportedSession{}, fmt.Errorf("open Claude transcript: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return source.ImportedSession{}, fmt.Errorf("stat Claude transcript: %w", err)
	}
	if info.Size() > source.MaxTranscriptBytes {
		return source.ImportedSession{}, fmt.Errorf("Claude transcript size %d exceeds %d bytes", info.Size(), source.MaxTranscriptBytes)
	}
	hasher := sha256.New()
	parsed, err := parseTranscript(ctx, io.TeeReader(file, hasher))
	if err != nil {
		return source.ImportedSession{}, fmt.Errorf("parse Claude transcript %q: %w", candidate.Path, err)
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
	sessionID := source.StableID("claude", parsed.meta.ID)
	for i := range parsed.messages {
		parsed.messages[i].SessionID = sessionID
	}
	return source.ImportedSession{
		Session: store.Session{
			ID: sessionID, Agent: "claude", NativeSessionID: parsed.meta.ID,
			SourcePath: candidate.Path, SourceSize: candidate.Size,
			SourceMTime: candidate.ModTime.UTC(), SourceHash: conversationNormalizerVersion + ":" + hashString(hasher),
			WorkingDirectory: parsed.meta.CWD, StartedAt: startedAt.UTC(),
			LastActiveAt: lastActiveAt.UTC(), AnalysisStatus: "none",
		},
		Messages: parsed.messages, NormalizerVersion: conversationNormalizerVersion,
	}, nil
}

// ResumeSpec returns the structured Claude Code resume invocation.
func (a *Adapter) ResumeSpec(session store.Session) (source.ResumeSpec, error) {
	if session.Agent != "claude" || session.NativeSessionID == "" {
		return source.ResumeSpec{}, errors.New("claude: invalid session")
	}
	return source.ResumeSpec{
		Agent: "claude", SessionID: session.NativeSessionID, CWD: session.WorkingDirectory,
		Executable: "claude", Args: []string{"--resume", session.NativeSessionID},
	}, nil
}

type metadata struct {
	ID        string
	CWD       string
	Timestamp time.Time
}

type parsedTranscript struct {
	meta     metadata
	messages []store.Message
}

type envelope struct {
	Type        string `json:"type"`
	SessionID   string `json:"sessionId"`
	CWD         string `json:"cwd"`
	Timestamp   string `json:"timestamp"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseTranscript(ctx context.Context, input io.Reader) (parsedTranscript, error) {
	reader := bufio.NewReader(input)
	var parsed parsedTranscript
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := source.ReadJSONLRecord(reader)
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			var record envelope
			if err := json.Unmarshal([]byte(trimmed), &record); err != nil {
				if errors.Is(readErr, io.EOF) || readerAtEOF(reader) {
					break
				}
				return parsedTranscript{}, fmt.Errorf("decode line %d: %w", lineNumber, err)
			}
			if err := consumeRecord(&parsed, record); err != nil {
				return parsedTranscript{}, fmt.Errorf("line %d: %w", lineNumber, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return parsedTranscript{}, err
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return parsedTranscript{}, readErr
		}
	}
	return parsed, nil
}

func consumeRecord(parsed *parsedTranscript, record envelope) error {
	timestamp, err := parseTimestamp(record.Timestamp)
	if err != nil {
		return err
	}
	if parsed.meta.ID == "" && record.SessionID != "" {
		parsed.meta.ID = record.SessionID
		parsed.meta.CWD = record.CWD
		parsed.meta.Timestamp = timestamp
	}
	if parsed.meta.CWD == "" && record.CWD != "" {
		parsed.meta.CWD = record.CWD
	}
	if record.IsMeta || record.IsSidechain {
		return nil
	}
	switch record.Type {
	case "user":
		return consumeUser(parsed, timestamp, record.Message.Content)
	case "assistant":
		return consumeAssistant(parsed, timestamp, record.Message.Content)
	}
	return nil
}

func consumeUser(parsed *parsedTranscript, timestamp time.Time, raw json.RawMessage) error {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = visibleUserText(text)
		if text != "" {
			appendMessage(parsed, timestamp, "user", text)
		}
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return fmt.Errorf("decode user content: %w", err)
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if text := visibleUserText(block.Text); text != "" {
				appendMessage(parsed, timestamp, "user", text)
			}
		}
	}
	return nil
}

func consumeAssistant(parsed *parsedTranscript, timestamp time.Time, raw json.RawMessage) error {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return fmt.Errorf("decode assistant content: %w", err)
	}
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			appendMessage(parsed, timestamp, "assistant", block.Text)
		}
	}
	return nil
}

func visibleUserText(text string) string {
	for _, marker := range []string{
		"<local-command-caveat>", "<local-command-stdout>", "<local-command-stderr>",
		"<command-name>", "<command-message>", "<command-args>",
		"<user-prompt-submit-hook>",
	} {
		if strings.Contains(text, marker) {
			return ""
		}
	}
	text = stripTaggedBlock(text, "system-reminder")
	text = stripTaggedBlock(text, "task-notification")
	return strings.TrimSpace(text)
}

func stripTaggedBlock(text, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	for {
		start := strings.Index(text, open)
		if start < 0 {
			return text
		}
		end := strings.Index(text[start+len(open):], close)
		if end < 0 {
			return text[:start]
		}
		end += start + len(open) + len(close)
		text = text[:start] + text[end:]
	}
}

func appendMessage(parsed *parsedTranscript, timestamp time.Time, role, text string) {
	parsed.messages = append(parsed.messages, store.Message{
		Sequence: len(parsed.messages), Timestamp: timestamp.UTC(), Role: role,
		Text: text,
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
				if errors.Is(readErr, io.EOF) || readerAtEOF(reader) {
					return metadata{}, errNoMetadata
				}
				return metadata{}, fmt.Errorf("decode line %d: %w", lineNumber, err)
			}
			if record.SessionID != "" {
				timestamp, err := parseTimestamp(record.Timestamp)
				if err != nil {
					return metadata{}, err
				}
				return metadata{ID: record.SessionID, CWD: record.CWD, Timestamp: timestamp}, nil
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

func readerAtEOF(reader *bufio.Reader) bool {
	_, err := reader.Peek(1)
	return errors.Is(err, io.EOF)
}

func parseTimestamp(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}
	return parsed.UTC(), nil
}

func hashString(hasher hash.Hash) string {
	return hex.EncodeToString(hasher.Sum(nil))
}

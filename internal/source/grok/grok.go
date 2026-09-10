// Package grok discovers and normalizes Grok Build session updates.
package grok

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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jaintarun/agent-history/internal/source"
	"github.com/jaintarun/agent-history/internal/store"
)

const (
	maxToolText      = 64 << 10
	truncationMarker = "\n[tool output truncated]"
)

var errNoMetadata = errors.New("grok: session metadata not found")

// Adapter reads Grok session history under one GROK_HOME directory.
type Adapter struct {
	home string
}

// New constructs a Grok source rooted at home.
func New(home string) *Adapter { return &Adapter{home: home} }

// DefaultHome returns GROK_HOME or the conventional ~/.grok path.
func DefaultHome() string {
	if home := os.Getenv("GROK_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".grok"
	}
	return filepath.Join(home, ".grok")
}

// Name returns the normalized source-agent name.
func (a *Adapter) Name() string { return "grok" }

// Discover finds authoritative top-level ACP update streams.
func (a *Adapter) Discover(ctx context.Context) ([]source.Candidate, error) {
	paths, err := filepath.Glob(filepath.Join(a.home, "sessions", "*", "*", "updates.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("discover Grok sessions: %w", err)
	}
	byID := make(map[string]source.Candidate)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat Grok transcript %q: %w", path, err)
		}
		if info.Size() > source.MaxTranscriptBytes {
			return nil, fmt.Errorf("transcript %q size %d exceeds %d bytes", path, info.Size(), source.MaxTranscriptBytes)
		}
		meta, err := readMetadata(filepath.Join(filepath.Dir(path), "summary.json"))
		if errors.Is(err, errNoMetadata) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read metadata %q: %w", path, err)
		}
		candidate := source.Candidate{
			Agent: "grok", NativeSessionID: meta.ID, Path: path,
			Size: info.Size(), ModTime: info.ModTime().UTC(),
		}
		current, exists := byID[meta.ID]
		if !exists || candidate.ModTime.After(current.ModTime) {
			byID[meta.ID] = candidate
		}
	}
	candidates := make([]source.Candidate, 0, len(byID))
	for _, candidate := range byID {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, nil
}

// Read parses an authoritative ACP update stream and retains visible
// conversation plus bounded completed tool activity.
func (a *Adapter) Read(ctx context.Context, candidate source.Candidate) (source.ImportedSession, error) {
	file, err := os.Open(candidate.Path)
	if err != nil {
		return source.ImportedSession{}, fmt.Errorf("open Grok transcript: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return source.ImportedSession{}, fmt.Errorf("stat Grok transcript: %w", err)
	}
	if info.Size() > source.MaxTranscriptBytes {
		return source.ImportedSession{}, fmt.Errorf("Grok transcript size %d exceeds %d bytes", info.Size(), source.MaxTranscriptBytes)
	}
	meta, err := readMetadata(filepath.Join(filepath.Dir(candidate.Path), "summary.json"))
	if err != nil {
		return source.ImportedSession{}, err
	}
	hasher := sha256.New()
	messages, err := parseUpdates(ctx, io.TeeReader(file, hasher))
	if err != nil {
		return source.ImportedSession{}, fmt.Errorf("parse Grok transcript %q: %w", candidate.Path, err)
	}
	if candidate.Size == 0 || candidate.ModTime.IsZero() {
		candidate.Size = info.Size()
		candidate.ModTime = info.ModTime().UTC()
	}
	startedAt, lastActiveAt := meta.CreatedAt, meta.LastActiveAt
	if (startedAt.IsZero() || lastActiveAt.IsZero()) && len(messages) != 0 {
		firstMessageAt, lastMessageAt := messages[0].Timestamp, messages[0].Timestamp
		for _, message := range messages[1:] {
			if message.Timestamp.Before(firstMessageAt) {
				firstMessageAt = message.Timestamp
			}
			if message.Timestamp.After(lastMessageAt) {
				lastMessageAt = message.Timestamp
			}
		}
		if startedAt.IsZero() {
			startedAt = firstMessageAt
		}
		if lastActiveAt.IsZero() {
			lastActiveAt = lastMessageAt
		}
	}
	if startedAt.IsZero() {
		startedAt = candidate.ModTime.UTC()
	}
	if lastActiveAt.IsZero() {
		lastActiveAt = candidate.ModTime.UTC()
	}
	sessionID := source.StableID("grok", meta.ID)
	for i := range messages {
		messages[i].SessionID = sessionID
	}
	return source.ImportedSession{
		Session: store.Session{
			ID: sessionID, Agent: "grok", NativeSessionID: meta.ID,
			SourcePath: candidate.Path, SourceSize: candidate.Size,
			SourceMTime: candidate.ModTime.UTC(), SourceHash: hashString(hasher),
			WorkingDirectory: meta.CWD, StartedAt: startedAt.UTC(),
			LastActiveAt: lastActiveAt.UTC(), AnalysisStatus: "none",
		},
		Messages: messages,
	}, nil
}

// ResumeSpec returns the structured Grok resume invocation.
func (a *Adapter) ResumeSpec(session store.Session) (source.ResumeSpec, error) {
	if session.Agent != "grok" || session.NativeSessionID == "" {
		return source.ResumeSpec{}, errors.New("grok: invalid session")
	}
	return source.ResumeSpec{
		Agent: "grok", SessionID: session.NativeSessionID, CWD: session.WorkingDirectory,
		Executable: "grok", Args: []string{"--resume", session.NativeSessionID},
	}, nil
}

type metadata struct {
	ID           string
	CWD          string
	CreatedAt    time.Time
	LastActiveAt time.Time
}

type envelope struct {
	Method    string `json:"method"`
	Timestamp int64  `json:"timestamp"`
	Params    struct {
		SessionID string `json:"sessionId"`
		Update    update `json:"update"`
	} `json:"params"`
}

type update struct {
	SessionUpdate     string          `json:"sessionUpdate"`
	Content           json.RawMessage `json:"content"`
	ToolCallID        string          `json:"toolCallId"`
	Title             string          `json:"title"`
	Status            string          `json:"status"`
	RawInput          json.RawMessage `json:"rawInput"`
	RawOutput         json.RawMessage `json:"rawOutput"`
	TargetPromptIndex *int            `json:"target_prompt_index"`
	Meta              struct {
		HideFromScrollback bool `json:"hideFromScrollback"`
		HostTurn           bool `json:"hostTurn"`
		PromptIndex        *int `json:"promptIndex"`
		Tool               struct {
			Name string `json:"name"`
		} `json:"x.ai/tool"`
	} `json:"_meta"`
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Content json.RawMessage `json:"content"`
}

type pendingTool struct {
	Timestamp time.Time
	Name      string
	Text      string
}

func parseUpdates(ctx context.Context, input io.Reader) ([]store.Message, error) {
	reader := bufio.NewReader(input)
	var messages []store.Message
	var rewinds rewindTracker
	pendingTools := make(map[string]pendingTool)
	completedTools := make(map[string]bool)
	suppressedTools := make(map[string]bool)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := source.ReadJSONLRecord(reader)
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			var record envelope
			if err := json.Unmarshal([]byte(trimmed), &record); err != nil {
				if errors.Is(readErr, io.EOF) || readerAtEOF(reader) {
					break
				}
				return nil, fmt.Errorf("decode line %d: %w", lineNumber, err)
			}
			if rewinds.observe(record, &messages) {
				clear(pendingTools)
				clear(completedTools)
				clear(suppressedTools)
			}
			if err := consumeUpdate(&messages, pendingTools, completedTools, suppressedTools, record); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return messages, nil
}

type rewindTracker struct {
	promptStarts       []int
	seenPromptIndex    bool
	inUser             bool
	currentPromptIndex *int
}

func (r *rewindTracker) observe(record envelope, messages *[]store.Message) bool {
	update := record.Params.Update
	if record.Method == "_x.ai/session/update" && update.SessionUpdate == "rewind_marker" && update.TargetPromptIndex != nil {
		target := *update.TargetPromptIndex
		if target >= 0 && target < len(r.promptStarts) {
			*messages = (*messages)[:r.promptStarts[target]]
			r.promptStarts = r.promptStarts[:target]
			r.endUserRun()
			return true
		}
		r.endUserRun()
		return false
	}
	if record.Method != "session/update" || update.SessionUpdate != "user_message_chunk" || update.Meta.HostTurn {
		r.endUserRun()
		return false
	}
	if r.startsPrompt(update.Meta.PromptIndex) {
		r.promptStarts = append(r.promptStarts, len(*messages))
	}
	return false
}

func (r *rewindTracker) startsPrompt(promptIndex *int) bool {
	if promptIndex != nil {
		r.seenPromptIndex = true
	}
	counts := !r.seenPromptIndex || promptIndex != nil
	newRun := !r.inUser
	if r.inUser && (r.seenPromptIndex || promptIndex != nil) {
		newRun = !sameIndex(promptIndex, r.currentPromptIndex)
	}
	if newRun {
		r.currentPromptIndex = promptIndex
	}
	r.inUser = true
	return newRun && counts
}

func (r *rewindTracker) endUserRun() {
	r.inUser = false
	r.currentPromptIndex = nil
}

func sameIndex(left, right *int) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func consumeUpdate(messages *[]store.Message, pendingTools map[string]pendingTool, completedTools, suppressedTools map[string]bool, record envelope) error {
	if record.Method != "session/update" || record.Params.Update.Meta.HideFromScrollback || record.Params.Update.Meta.HostTurn {
		update := record.Params.Update
		if update.ToolCallID != "" && (update.SessionUpdate == "tool_call" || update.SessionUpdate == "tool_call_update") {
			suppressedTools[update.ToolCallID] = true
			delete(pendingTools, update.ToolCallID)
		}
		return nil
	}
	if record.Timestamp <= 0 {
		return errors.New("update timestamp is missing")
	}
	timestamp := time.Unix(record.Timestamp, 0).UTC()
	switch record.Params.Update.SessionUpdate {
	case "user_message_chunk":
		if record.Params.Update.Meta.HideFromScrollback {
			return nil
		}
		if text := messageText(record.Params.Update.Content); text != "" {
			appendMessage(messages, timestamp, "user", text, "")
		}
	case "agent_message_chunk":
		if text := messageText(record.Params.Update.Content); text != "" {
			appendMessage(messages, timestamp, "assistant", text, "")
		}
	case "tool_call":
		if record.Params.Update.ToolCallID == "" {
			return nil
		}
		toolName := strings.TrimSpace(record.Params.Update.Meta.Tool.Name)
		if toolName == "" {
			toolName = "tool"
		}
		text := strings.TrimSpace(toolName + " " + rawText(record.Params.Update.RawInput))
		if text == toolName {
			text = ""
		}
		pendingTools[record.Params.Update.ToolCallID] = pendingTool{
			Timestamp: timestamp, Name: toolName, Text: boundToolText(text),
		}
	case "tool_call_update":
		if suppressedTools[record.Params.Update.ToolCallID] {
			return nil
		}
		if record.Params.Update.Status != "completed" && record.Params.Update.Status != "failed" {
			return nil
		}
		if record.Params.Update.ToolCallID != "" && completedTools[record.Params.Update.ToolCallID] {
			return nil
		}
		pending := pendingTools[record.Params.Update.ToolCallID]
		toolName := pending.Name
		if toolName == "" {
			toolName = strings.TrimSpace(record.Params.Update.Meta.Tool.Name)
		}
		if toolName == "" {
			toolName = "tool"
		}
		if pending.Text != "" {
			appendMessage(messages, pending.Timestamp, "tool", pending.Text, toolName)
		}
		if text := toolResultText(record.Params.Update.RawOutput, record.Params.Update.Content); text != "" {
			appendMessage(messages, timestamp, "tool", boundToolText(text), toolName)
		}
		if record.Params.Update.ToolCallID != "" {
			completedTools[record.Params.Update.ToolCallID] = true
			delete(pendingTools, record.Params.Update.ToolCallID)
		}
	}
	return nil
}

func messageText(raw json.RawMessage) string {
	var block contentBlock
	if json.Unmarshal(raw, &block) != nil || block.Type != "text" {
		return ""
	}
	return strings.TrimSpace(block.Text)
}

func toolResultText(rawOutput, rawContent json.RawMessage) string {
	var output struct {
		OutputForPrompt string `json:"output_for_prompt"`
	}
	if json.Unmarshal(rawOutput, &output) == nil && strings.TrimSpace(output.OutputForPrompt) != "" {
		return strings.TrimSpace(output.OutputForPrompt)
	}
	if text := nestedContentText(rawContent); text != "" {
		return text
	}
	return strings.TrimSpace(rawText(rawOutput))
}

func nestedContentText(raw json.RawMessage) string {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			texts = append(texts, block.Text)
			continue
		}
		var nested contentBlock
		if json.Unmarshal(block.Content, &nested) == nil && nested.Type == "text" && nested.Text != "" {
			texts = append(texts, nested.Text)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

func appendMessage(messages *[]store.Message, timestamp time.Time, role, text, toolName string) {
	*messages = append(*messages, store.Message{
		Sequence: len(*messages), Timestamp: timestamp.UTC(), Role: role,
		Text: text, ToolName: toolName,
	})
}

func readMetadata(path string) (metadata, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return metadata{}, errNoMetadata
	}
	if err != nil {
		return metadata{}, err
	}
	var summary struct {
		CreatedAt    string `json:"created_at"`
		LastActiveAt string `json:"last_active_at"`
		Info         struct {
			ID  string `json:"id"`
			CWD string `json:"cwd"`
		} `json:"info"`
	}
	if err := json.Unmarshal(content, &summary); err != nil {
		return metadata{}, err
	}
	if summary.Info.ID == "" {
		return metadata{}, errNoMetadata
	}
	createdAt, err := parseTime(summary.CreatedAt)
	if err != nil {
		return metadata{}, fmt.Errorf("parse created_at: %w", err)
	}
	lastActiveAt, err := parseTime(summary.LastActiveAt)
	if err != nil {
		return metadata{}, fmt.Errorf("parse last_active_at: %w", err)
	}
	return metadata{ID: summary.Info.ID, CWD: summary.Info.CWD, CreatedAt: createdAt, LastActiveAt: lastActiveAt}, nil
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
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
	if json.Compact(&compact, raw) == nil {
		return compact.String()
	}
	return ""
}

func boundToolText(text string) string {
	if len(text) <= maxToolText {
		return text
	}
	limit := maxToolText - len(truncationMarker)
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit] + truncationMarker
}

func readerAtEOF(reader *bufio.Reader) bool {
	_, err := reader.Peek(1)
	return errors.Is(err, io.EOF)
}

func hashString(hasher hash.Hash) string {
	return hex.EncodeToString(hasher.Sum(nil))
}

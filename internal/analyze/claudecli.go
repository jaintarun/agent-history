package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCLI invokes Claude Code print mode using existing CLI authentication.
type ClaudeCLI struct {
	Executable     string
	Timeout        time.Duration
	MaxOutputBytes int
}

// NewClaudeCLI constructs the production Claude Code CLI analyzer.
func NewClaudeCLI(executable string) *ClaudeCLI {
	if executable == "" {
		executable = "claude"
	}
	return &ClaudeCLI{
		Executable: executable, Timeout: defaultAnalyzerTimeout,
		MaxOutputBytes: defaultOutputLimit,
	}
}

// Generate runs one nonpersistent, schema-constrained analysis request.
func (c *ClaudeCLI) Generate(ctx context.Context, model string, request StructuredRequest) (json.RawMessage, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("claude-cli: model is empty")
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return nil, errors.New("claude-cli: prompt is empty")
	}
	if !json.Valid(request.Schema) {
		return nil, errors.New("claude-cli: output schema is invalid JSON")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultAnalyzerTimeout
	}
	limit := c.MaxOutputBytes
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tempDir, err := os.MkdirTemp("", "agent-history-analysis-*")
	if err != nil {
		return nil, fmt.Errorf("claude-cli: create temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	args := []string{
		"--safe-mode", "-p", "--no-session-persistence", "--tools", "",
		"--disable-slash-commands", "--permission-mode", "dontAsk", "--max-turns", "1",
		"--output-format", "json", "--json-schema", string(request.Schema), "--model", model,
	}
	command := exec.CommandContext(runContext, c.Executable, args...)
	command.Dir = tempDir
	command.Stdin = strings.NewReader(request.Prompt)
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if runContext.Err() != nil {
			return nil, fmt.Errorf("claude-cli: %w", runContext.Err())
		}
		return nil, fmt.Errorf("claude-cli: command failed: %w: %s", err, stderr.String())
	}
	if stdout.truncated {
		return nil, fmt.Errorf("claude-cli: result exceeds %d bytes", limit)
	}
	var envelope struct {
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(stdout.buffer.Bytes(), &envelope); err != nil {
		return nil, fmt.Errorf("claude-cli: result is invalid JSON: %w", err)
	}
	if len(envelope.StructuredOutput) == 0 || string(envelope.StructuredOutput) == "null" {
		return nil, errors.New("claude-cli: structured_output is missing")
	}
	if !json.Valid(envelope.StructuredOutput) {
		return nil, errors.New("claude-cli: structured_output is invalid JSON")
	}
	return envelope.StructuredOutput, nil
}

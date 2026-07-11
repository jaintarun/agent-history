package analyze

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultAnalyzerTimeout = 5 * time.Minute
	defaultOutputLimit     = 256 << 10
)

// CodexCLI invokes codex exec using existing CLI authentication.
type CodexCLI struct {
	Executable     string
	Timeout        time.Duration
	MaxOutputBytes int
}

// NewCodexCLI constructs the production Codex CLI analyzer.
func NewCodexCLI(executable string) *CodexCLI {
	if executable == "" {
		executable = "codex"
	}
	return &CodexCLI{
		Executable: executable, Timeout: defaultAnalyzerTimeout,
		MaxOutputBytes: defaultOutputLimit,
	}
}

// Generate runs one ephemeral schema-constrained analysis request.
func (c *CodexCLI) Generate(ctx context.Context, model string, request StructuredRequest) (json.RawMessage, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("codex-cli: model is empty")
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return nil, errors.New("codex-cli: prompt is empty")
	}
	if !json.Valid(request.Schema) {
		return nil, errors.New("codex-cli: output schema is invalid JSON")
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
		return nil, fmt.Errorf("codex-cli: create temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	schemaPath := filepath.Join(tempDir, "schema.json")
	outputPath := filepath.Join(tempDir, "result.json")
	if err := os.WriteFile(schemaPath, request.Schema, 0o600); err != nil {
		return nil, fmt.Errorf("codex-cli: write output schema: %w", err)
	}

	args := []string{
		"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "--sandbox", "read-only", "--color", "never",
		"--cd", tempDir, "--output-schema", schemaPath,
		"--output-last-message", outputPath, "--model", model, "-",
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
			return nil, fmt.Errorf("codex-cli: %w", runContext.Err())
		}
		return nil, fmt.Errorf("codex-cli: command failed: %w: %s", err, stderr.String())
	}
	result, err := readBoundedFile(outputPath, limit)
	if err != nil {
		return nil, fmt.Errorf("codex-cli: read result: %w", err)
	}
	if !json.Valid(result) {
		return nil, errors.New("codex-cli: result is not valid JSON")
	}
	return json.RawMessage(result), nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
			b.truncated = true
		}
		_, _ = b.buffer.Write(value)
	} else if len(value) != 0 {
		b.truncated = true
	}
	return originalLength, nil
}

func (b *limitedBuffer) String() string {
	if b.truncated {
		return b.buffer.String() + " [truncated]"
	}
	return b.buffer.String()
}

func readBoundedFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	result, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(result) > limit {
		return nil, fmt.Errorf("result exceeds %d bytes", limit)
	}
	return result, nil
}

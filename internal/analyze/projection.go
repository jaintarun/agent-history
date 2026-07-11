package analyze

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tarunjain/agent-history/internal/store"
)

const maxProjectedMessageChars = 8_000

// Turn groups one user request with related assistant and tool activity.
type Turn struct {
	StartSequence int
	EndSequence   int
	StartedAt     time.Time
	EndedAt       time.Time
	UserText      string
	Projection    string
	Messages      []store.Message
}

// BuildTurns groups normalized records without changing their stored content.
func BuildTurns(messages []store.Message) []Turn {
	var turns []Turn
	var current *Turn
	finish := func() {
		if current == nil {
			return
		}
		current.Projection = CompactTurn(*current)
		turns = append(turns, *current)
		current = nil
	}
	for _, message := range messages {
		if message.Role == "user" && current != nil {
			finish()
		}
		if current == nil {
			current = &Turn{
				StartSequence: message.Sequence, EndSequence: message.Sequence,
				StartedAt: message.Timestamp, EndedAt: message.Timestamp,
			}
		}
		current.EndSequence = message.Sequence
		current.EndedAt = message.Timestamp
		current.Messages = append(current.Messages, message)
		if message.Role == "user" {
			current.UserText = message.Text
		}
	}
	finish()
	return turns
}

// CompactTurn builds deterministic bounded model input while retaining the
// exact normalized text in SQLite for search.
func CompactTurn(turn Turn) string {
	var result strings.Builder
	for _, message := range turn.Messages {
		text := message.Text
		if message.Role == "tool" {
			text = compactToolText(text)
		}
		text = boundText(text, maxProjectedMessageChars)
		label := message.Role
		if message.ToolName != "" {
			label += ":" + message.ToolName
		}
		fmt.Fprintf(&result, "[message %d %s]\n%s\n", message.Sequence, label, text)
	}
	return strings.TrimSpace(result.String())
}

func compactToolText(text string) string {
	if strings.Contains(text, "diff --git ") {
		return compactDiff(text)
	}
	lines := strings.Split(text, "\n")
	var compacted []string
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		count := j - i
		if count >= 3 && strings.TrimSpace(lines[i]) != "" {
			compacted = append(compacted, fmt.Sprintf("%s [repeated %d times]", lines[i], count))
		} else {
			compacted = append(compacted, lines[i:j]...)
		}
		i = j
	}
	return strings.Join(compacted, "\n")
}

func compactDiff(text string) string {
	seen := make(map[string]bool)
	var files []string
	additions, deletions := 0, 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				file := strings.TrimPrefix(fields[3], "b/")
				if !seen[file] {
					seen[file] = true
					files = append(files, file)
				}
			}
			continue
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			additions++
		}
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			deletions++
		}
	}
	sort.Strings(files)
	return fmt.Sprintf("diff files: %s; additions: %d; deletions: %d", strings.Join(files, ", "), additions, deletions)
}

func boundText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !unicode.IsSpace(rune(text[end-1])) {
		end--
	}
	if end < limit/2 {
		end = limit
	}
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "\n[projection truncated]"
}

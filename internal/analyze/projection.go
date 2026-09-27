package analyze

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jaintarun/agent-history/internal/store"
)

const (
	maxProjectedTurnChars     = 8_000
	userProjectionBudget      = 2_400
	assistantProjectionBudget = 2_800
)

// Turn groups one user request with related assistant activity.
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
	var users, assistants []store.Message
	for _, message := range turn.Messages {
		switch message.Role {
		case "user":
			users = append(users, message)
		case "assistant":
			assistants = append(assistants, message)
		}
	}
	var sections []string
	if projected := projectMessages(users, userProjectionBudget); projected != "" {
		sections = append(sections, projected)
	}
	selectedAssistants := selectEdgeMessages(assistants, 1, 3)
	if projected := projectMessages(selectedAssistants, assistantProjectionBudget); projected != "" {
		sections = append(sections, projected)
	}
	return strings.TrimSpace(boundText(strings.Join(sections, "\n"), maxProjectedTurnChars))
}

func projectMessages(messages []store.Message, budget int) string {
	if len(messages) == 0 || budget <= 0 {
		return ""
	}
	perMessage := max(80, budget/len(messages)-80)
	var result strings.Builder
	for _, message := range messages {
		fmt.Fprintf(&result, "[message %d %s]\n%s\n", message.Sequence, message.Role, boundText(message.Text, perMessage))
	}
	return strings.TrimSpace(boundText(result.String(), budget))
}

func selectEdgeMessages(messages []store.Message, first, last int) []store.Message {
	if len(messages) <= first+last {
		return messages
	}
	result := append([]store.Message(nil), messages[:first]...)
	return append(result, messages[len(messages)-last:]...)
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

package analyze

import (
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const ambiguousBoundaryScore = 2

// BoundaryDecision is the deterministic evidence score between adjacent turns.
type BoundaryDecision struct {
	Score     int
	Strong    bool
	Ambiguous bool
	Reasons   []string
}

// ScoreBoundary combines explicit goal, repository, completion, vocabulary,
// and idle evidence. An idle gap alone is never enough to split a topic.
func ScoreBoundary(before, after Turn) BoundaryDecision {
	var decision BoundaryDecision
	lowerAfter := strings.ToLower(strings.TrimSpace(after.UserText))
	for _, prefix := range []string{"new task", "new goal", "switch gears", "unrelated task", "different task"} {
		if strings.HasPrefix(lowerAfter, prefix) {
			decision.Score += 3
			decision.Reasons = append(decision.Reasons, "explicit new goal")
			break
		}
	}
	beforeRepos, afterRepos := repositoryHints(before), repositoryHints(after)
	if len(beforeRepos) != 0 && len(afterRepos) != 0 && !setsOverlap(beforeRepos, afterRepos) {
		decision.Score += 2
		decision.Reasons = append(decision.Reasons, "repository shift")
	}
	lowerBefore := strings.ToLower(before.Projection)
	for _, marker := range []string{"complete", "completed", "passes", "fixed", "done"} {
		if strings.Contains(lowerBefore, marker) {
			decision.Score++
			decision.Reasons = append(decision.Reasons, "previous objective completed")
			break
		}
	}
	if vocabularyShift(before.UserText+" "+before.Projection, after.UserText+" "+after.Projection) {
		decision.Score++
		decision.Reasons = append(decision.Reasons, "vocabulary shift")
	}
	if !before.EndedAt.IsZero() && !after.StartedAt.IsZero() && after.StartedAt.Sub(before.EndedAt) >= 12*time.Hour && decision.Score > 0 {
		decision.Score++
		decision.Reasons = append(decision.Reasons, "idle gap with topic signal")
	}
	decision.Strong = decision.Score >= 3
	decision.Ambiguous = !decision.Strong && decision.Score >= ambiguousBoundaryScore
	return decision
}

func repositoryHints(turn Turn) map[string]bool {
	result := make(map[string]bool)
	for _, field := range strings.Fields(turn.UserText + " " + turn.Projection) {
		field = strings.Trim(field, "`'\".,:;()[]{}")
		if !strings.HasPrefix(field, "/") {
			continue
		}
		clean := filepath.Clean(field)
		parts := strings.Split(clean, string(filepath.Separator))
		if len(parts) >= 4 {
			result[strings.Join(parts[:4], string(filepath.Separator))] = true
		}
	}
	return result
}

func setsOverlap(left, right map[string]bool) bool {
	for value := range left {
		if right[value] {
			return true
		}
	}
	return false
}

func vocabularyShift(left, right string) bool {
	leftWords, rightWords := significantWords(left), significantWords(right)
	if len(leftWords) < 4 || len(rightWords) < 4 {
		return false
	}
	intersection := 0
	for word := range leftWords {
		if rightWords[word] {
			intersection++
		}
	}
	union := len(leftWords) + len(rightWords) - intersection
	return union > 0 && float64(intersection)/float64(union) < 0.12
}

func significantWords(text string) map[string]bool {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
	})
	stop := map[string]bool{"the": true, "and": true, "that": true, "this": true, "with": true, "from": true, "for": true, "into": true, "will": true, "now": true}
	result := make(map[string]bool)
	for _, word := range words {
		if len(word) >= 3 && !stop[word] {
			result[word] = true
		}
	}
	return result
}

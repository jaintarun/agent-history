// Package evaluation runs explicitly invoked analyzer quality checks.
package evaluation

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jaintarun/agent-history/internal/analyze"
	"github.com/jaintarun/agent-history/internal/store"
)

//go:embed corpus.json
var corpusFS embed.FS

type fixture struct {
	Name               string           `json:"name"`
	InitialCount       int              `json:"initial_count"`
	ExpectedTitleTerms []string         `json:"expected_title_terms"`
	ExpectedTopicEnds  []int            `json:"expected_topic_ends"`
	CoverageTerms      []string         `json:"coverage_terms"`
	ForbiddenTerms     []string         `json:"forbidden_terms"`
	Messages           []fixtureMessage `json:"messages"`
}

type fixtureMessage struct {
	Hours    int    `json:"hours"`
	Role     string `json:"role"`
	ToolName string `json:"tool_name"`
	Text     string `json:"text"`
}

// Report is the machine-readable evaluation result.
type Report struct {
	Model  string       `json:"model"`
	Cases  []CaseReport `json:"cases"`
	Passed bool         `json:"passed"`
}

// CaseReport contains prose-independent quality and cost measurements.
type CaseReport struct {
	Name                   string  `json:"name"`
	TitleSpecificity       float64 `json:"title_specificity"`
	ExpectedTopics         int     `json:"expected_topics"`
	ActualTopics           int     `json:"actual_topics"`
	BoundaryF1             float64 `json:"boundary_f1"`
	SummaryCoverage        float64 `json:"summary_coverage"`
	EvidenceGrounded       bool    `json:"evidence_grounded"`
	HiddenTermsExcluded    bool    `json:"hidden_terms_excluded"`
	InitialCalls           int     `json:"initial_calls"`
	InitialInputChars      int     `json:"initial_input_chars"`
	AppendCalls            int     `json:"append_calls"`
	AppendInputChars       int     `json:"append_input_chars"`
	ApproximateInputTokens int     `json:"approximate_input_tokens"`
	AppendEfficient        bool    `json:"append_efficient"`
	Passed                 bool    `json:"passed"`
}

// Run analyzes the embedded sanitized corpus with analyzer.
func Run(ctx context.Context, analyzer analyze.Analyzer, model string) (Report, error) {
	fixtures, err := loadCorpus()
	if err != nil {
		return Report{}, err
	}
	report := Report{Model: model, Passed: true}
	for index, test := range fixtures {
		result, err := runFixture(ctx, analyzer, model, index, test)
		if err != nil {
			return Report{}, fmt.Errorf("evaluate %s: %w", test.Name, err)
		}
		report.Cases = append(report.Cases, result)
		report.Passed = report.Passed && result.Passed
	}
	return report, nil
}

func runFixture(ctx context.Context, analyzer analyze.Analyzer, model string, index int, test fixture) (CaseReport, error) {
	directory, err := os.MkdirTemp("", "agent-history-eval-*")
	if err != nil {
		return CaseReport{}, err
	}
	defer os.RemoveAll(directory)
	database, err := store.Open(ctx, filepath.Join(directory, "history.db"))
	if err != nil {
		return CaseReport{}, err
	}
	defer database.Close()
	messages := fixtureMessages(test.Messages)
	initialCount := test.InitialCount
	if initialCount <= 0 || initialCount > len(messages) {
		initialCount = len(messages)
	}
	session := store.Session{
		ID: fmt.Sprintf("evaluation-%d", index), Agent: "codex", NativeSessionID: fmt.Sprintf("evaluation-%d", index),
		SourcePath: filepath.Join(directory, "fixture.jsonl"), SourceSize: int64(initialCount),
		SourceMTime: messages[0].Timestamp, SourceHash: "initial", WorkingDirectory: "/work/evaluation",
		StartedAt: messages[0].Timestamp, LastActiveAt: messages[initialCount-1].Timestamp,
	}
	if _, err := database.ImportSession(ctx, session, messages[:initialCount]); err != nil {
		return CaseReport{}, err
	}
	options := analyze.Options{
		Provider: "evaluation", Model: model, PromptVersion: "v1", NormalizerVersion: "v2",
		LeafTargetChars: 900, RollupFanout: 4,
	}
	engine := analyze.NewEngine(database, map[string]analyze.Analyzer{"evaluation": analyzer})
	initial, err := engine.Analyze(ctx, session.ID, options)
	if err != nil {
		return CaseReport{}, err
	}
	final := initial
	var appended analyze.Result
	if initialCount < len(messages) {
		session.SourceSize = int64(len(messages))
		session.SourceHash = "appended"
		session.LastActiveAt = messages[len(messages)-1].Timestamp
		if _, err := database.ImportSession(ctx, session, messages); err != nil {
			return CaseReport{}, err
		}
		appended, err = engine.Analyze(ctx, session.ID, options)
		if err != nil {
			return CaseReport{}, err
		}
		final = appended
	}
	detail, err := database.GetSession(ctx, session.ID)
	if err != nil {
		return CaseReport{}, err
	}
	visible := detail.Session.Title + "\n" + detail.Session.Summary
	for _, segment := range detail.Segments {
		visible += "\n" + segment.Title + "\n" + segment.Summary + "\n" + segment.Detail
	}
	expectedTopics := len(test.ExpectedTopicEnds) + 1
	result := CaseReport{
		Name: test.Name, TitleSpecificity: termCoverage(detail.Session.Title, test.ExpectedTitleTerms),
		ExpectedTopics: expectedTopics, ActualTopics: len(detail.Segments),
		BoundaryF1:          boundaryF1(detail.Segments, test.ExpectedTopicEnds),
		SummaryCoverage:     termCoverage(visible, test.CoverageTerms),
		EvidenceGrounded:    evidenceGrounded(detail.Segments),
		HiddenTermsExcluded: !containsAny(visible, test.ForbiddenTerms),
		InitialCalls:        initial.Calls, InitialInputChars: initial.InputChars,
		AppendCalls: appended.Calls, AppendInputChars: appended.InputChars,
		ApproximateInputTokens: (initial.InputChars + appended.InputChars + 3) / 4,
	}
	result.AppendEfficient = initialCount == len(messages) || (appended.Calls <= initial.Calls+1 && appended.InputChars <= initial.InputChars*3)
	result.Passed = result.TitleSpecificity >= .5 && result.ActualTopics == result.ExpectedTopics && result.BoundaryF1 >= .8 && result.SummaryCoverage >= .6 && result.EvidenceGrounded && result.HiddenTermsExcluded && result.AppendEfficient && final.Calls > 0
	return result, nil
}

func loadCorpus() ([]fixture, error) {
	content, err := corpusFS.ReadFile("corpus.json")
	if err != nil {
		return nil, err
	}
	var fixtures []fixture
	if err := json.Unmarshal(content, &fixtures); err != nil {
		return nil, err
	}
	return fixtures, nil
}

func fixtureMessages(input []fixtureMessage) []store.Message {
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	result := make([]store.Message, 0, len(input))
	for sequence, message := range input {
		result = append(result, store.Message{Sequence: sequence, Timestamp: base.Add(time.Duration(message.Hours) * time.Hour), Role: message.Role, ToolName: message.ToolName, Text: message.Text})
	}
	return result
}

func termCoverage(text string, terms []string) float64 {
	if len(terms) == 0 {
		return 1
	}
	lower := strings.ToLower(text)
	found := 0
	for _, term := range terms {
		if strings.Contains(lower, strings.ToLower(term)) {
			found++
		}
	}
	return float64(found) / float64(len(terms))
}

func boundaryF1(segments []store.Segment, expected []int) float64 {
	actual := make(map[int]bool)
	for index, segment := range segments {
		if index < len(segments)-1 {
			actual[segment.EndSequence] = true
		}
	}
	wanted := make(map[int]bool, len(expected))
	for _, boundary := range expected {
		wanted[boundary] = true
	}
	if len(actual) == 0 && len(wanted) == 0 {
		return 1
	}
	matches := 0
	for boundary := range actual {
		if wanted[boundary] {
			matches++
		}
	}
	if matches == 0 {
		return 0
	}
	precision := float64(matches) / float64(len(actual))
	recall := float64(matches) / float64(len(wanted))
	return 2 * precision * recall / (precision + recall)
}

func evidenceGrounded(segments []store.Segment) bool {
	if len(segments) == 0 {
		return false
	}
	for _, segment := range segments {
		if !strings.Contains(segment.Detail, "Evidence messages:") {
			return false
		}
	}
	return true
}

func containsAny(text string, terms []string) bool {
	lower := strings.ToLower(text)
	for _, term := range terms {
		if strings.Contains(lower, strings.ToLower(term)) {
			return true
		}
	}
	return false
}

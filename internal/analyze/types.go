package analyze

import (
	"context"
	"encoding/json"
)

// RequestKind identifies one bounded stage in the summary tree.
type RequestKind string

const (
	RequestLeaf     RequestKind = "leaf"
	RequestBoundary RequestKind = "boundary"
	RequestRollup   RequestKind = "rollup"
	RequestTopic    RequestKind = "topic"
	RequestSession  RequestKind = "session"
	RequestTitle    RequestKind = "title"
)

// StructuredRequest contains bounded prompt input and the required response
// schema. Transcript text inside Prompt is always untrusted data.
type StructuredRequest struct {
	Kind   RequestKind
	Prompt string
	Schema json.RawMessage
}

// Analyzer is the minimal external model-provider boundary.
type Analyzer interface {
	Generate(context.Context, string, StructuredRequest) (json.RawMessage, error)
}

// Options controls analysis provenance and tested internal tree limits.
type Options struct {
	Provider          string
	Model             string
	PromptVersion     string
	NormalizerVersion string
	LeafTargetChars   int
	RollupFanout      int
	Full              bool
}

// Result reports the persisted analysis plus model-input accounting.
type Result struct {
	Analysis   AnalysisResult
	Calls      int
	InputChars int
}

// AnalysisResult is the visible three-level result.
type AnalysisResult struct {
	Title   string        `json:"title"`
	Summary string        `json:"summary"`
	Topics  []TopicResult `json:"topics"`
}

// TopicResult is one chronological visible topic.
type TopicResult struct {
	Title        string `json:"title"`
	StartMessage int    `json:"start_message"`
	EndMessage   int    `json:"end_message"`
	Summary      string `json:"summary"`
	Detail       string `json:"detail"`
	Evidence     []int  `json:"evidence"`
}

type leafSummary struct {
	Title    string   `json:"title"`
	Goal     string   `json:"goal"`
	Summary  string   `json:"summary"`
	Detail   string   `json:"detail"`
	Outcome  string   `json:"outcome"`
	Entities []string `json:"entities"`
	Files    []string `json:"files"`
	Errors   []string `json:"errors"`
	Evidence []int    `json:"evidence"`
}

type rollupSummary struct {
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail"`
	Evidence []int  `json:"evidence"`
}

type sessionSummary struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

type titleSummary struct {
	Title string `json:"title"`
}

type modelBoundary struct {
	SameTopic  bool    `json:"same_topic"`
	Confidence float64 `json:"confidence"`
	NewTitle   string  `json:"new_title"`
}

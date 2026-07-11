package store

import "time"

// Session is the normalized metadata and current analysis state for one source
// transcript.
type Session struct {
	ID                      string
	Agent                   string
	NativeSessionID         string
	SourcePath              string
	SourceSize              int64
	SourceMTime             time.Time
	SourceHash              string
	WorkingDirectory        string
	Title                   string
	Summary                 string
	TopicCount              int
	StartedAt               time.Time
	LastActiveAt            time.Time
	AnalysisStatus          string
	AnalysisError           string
	AnalysisProvider        string
	AnalysisModel           string
	AnalysisPromptVersion   string
	AnalyzedAt              time.Time
	AnalyzedHash            string
	AnalyzedThroughSequence *int
	AnalyzedThroughAt       time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Message is one visible normalized transcript record.
type Message struct {
	SessionID string
	Sequence  int
	Timestamp time.Time
	Role      string
	Text      string
	ToolName  string
}

// Segment is one chronological topic chapter in the visible analysis.
type Segment struct {
	SessionID     string
	Position      int
	StartSequence int
	EndSequence   int
	Title         string
	Summary       string
	Detail        string
}

// SummaryNode is one reusable internal leaf or rollup in the analysis tree.
type SummaryNode struct {
	ID                string
	SessionID         string
	ParentID          *string
	Kind              string
	Position          int
	StartSequence     int
	EndSequence       int
	InputHash         string
	SummaryJSON       string
	Sealed            bool
	Provider          string
	Model             string
	PromptVersion     string
	NormalizerVersion string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// NodeCacheKey identifies a reusable summary node.
type NodeCacheKey struct {
	SessionID         string
	Kind              string
	StartSequence     int
	EndSequence       int
	InputHash         string
	Provider          string
	Model             string
	PromptVersion     string
	NormalizerVersion string
}

// Analysis is a complete replacement for the visible and internal generated
// analysis of a session.
type Analysis struct {
	Title                   string
	Summary                 string
	Status                  string
	Error                   string
	Provider                string
	Model                   string
	PromptVersion           string
	AnalyzedAt              time.Time
	AnalyzedHash            string
	AnalyzedThroughSequence *int
	AnalyzedThroughAt       time.Time
	Segments                []Segment
	Nodes                   []SummaryNode
}

// SessionDetail contains the normalized conversation and current analysis.
type SessionDetail struct {
	Session  Session
	Messages []Message
	Segments []Segment
}

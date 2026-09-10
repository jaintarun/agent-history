package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/jaintarun/agent-history/internal/store"
)

// Message is a normalized visible transcript message.
type Message = store.Message

// Candidate is a transcript discovered by a provider adapter.
type Candidate struct {
	Agent           string
	NativeSessionID string
	Path            string
	Size            int64
	ModTime         time.Time
	Archived        bool
	// Excluded removes a previously imported provider-internal session.
	Excluded bool
}

// ImportedSession is one completely parsed provider transcript.
type ImportedSession struct {
	Session  store.Session
	Messages []store.Message
}

// ResumeSpec is a trusted structured command produced by a source adapter.
type ResumeSpec struct {
	Agent      string
	SessionID  string
	CWD        string
	Executable string
	Args       []string
}

// Source discovers, reads, and describes sessions for one provider.
type Source interface {
	Name() string
	Discover(context.Context) ([]Candidate, error)
	Read(context.Context, Candidate) (ImportedSession, error)
	ResumeSpec(store.Session) (ResumeSpec, error)
}

// StableID returns the deterministic internal ID for a native provider session.
func StableID(agent, nativeSessionID string) string {
	sum := sha256.Sum256([]byte(agent + "\x00" + nativeSessionID))
	return hex.EncodeToString(sum[:16])
}

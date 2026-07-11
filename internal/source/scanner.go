package source

import (
	"context"
	"fmt"
	"time"

	"github.com/tarunjain/agent-history/internal/store"
)

// ScanReport summarizes one synchronous transcript scan.
type ScanReport struct {
	Discovered   int
	Imported     int
	MetadataOnly int
	Skipped      int
}

// Scanner imports normalized sessions from configured provider adapters.
type Scanner struct {
	store   *store.Store
	sources []Source
}

// NewScanner constructs a transcript scanner.
func NewScanner(database *store.Store, sources ...Source) *Scanner {
	return &Scanner{store: database, sources: sources}
}

// Scan imports all sources matching agent. The value "all" selects every
// configured source.
func (s *Scanner) Scan(ctx context.Context, agent string) (ScanReport, error) {
	var report ScanReport
	matched := false
	for _, adapter := range s.sources {
		if agent != "all" && agent != adapter.Name() {
			continue
		}
		matched = true
		candidates, err := adapter.Discover(ctx)
		if err != nil {
			return report, fmt.Errorf("discover %s sessions: %w", adapter.Name(), err)
		}
		report.Discovered += len(candidates)
		for _, candidate := range candidates {
			state, exists, err := s.store.SessionSourceState(ctx, adapter.Name(), candidate.NativeSessionID)
			if err != nil {
				return report, err
			}
			if exists && sourceUnchanged(state, candidate) {
				report.Skipped++
				continue
			}
			imported, err := adapter.Read(ctx, candidate)
			if err != nil {
				return report, fmt.Errorf("read %s session %q: %w", adapter.Name(), candidate.Path, err)
			}
			result, err := s.store.ImportSession(ctx, imported.Session, imported.Messages)
			if err != nil {
				return report, fmt.Errorf("store %s session %q: %w", adapter.Name(), candidate.Path, err)
			}
			if result.Changed || !exists {
				report.Imported++
			} else {
				report.MetadataOnly++
			}
		}
	}
	if !matched {
		return report, fmt.Errorf("unknown transcript agent %q", agent)
	}
	return report, nil
}

func sourceUnchanged(state store.SourceState, candidate Candidate) bool {
	return state.SourcePath == candidate.Path &&
		state.SourceSize == candidate.Size &&
		state.SourceTime.Equal(candidate.ModTime.UTC().Truncate(time.Nanosecond))
}

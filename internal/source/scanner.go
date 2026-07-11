package source

import (
	"context"
	"fmt"
	"os"
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
	store    *store.Store
	sources  []Source
	scanSlot chan struct{}
}

// Rescan reads one stored session from its current source path regardless of
// discovery fast-path metadata.
func (s *Scanner) Rescan(ctx context.Context, sessionID string) (store.ImportResult, error) {
	if err := s.acquire(ctx); err != nil {
		return store.ImportResult{}, err
	}
	defer s.release()
	detail, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return store.ImportResult{}, err
	}
	var adapter Source
	for _, candidate := range s.sources {
		if candidate.Name() == detail.Session.Agent {
			adapter = candidate
			break
		}
	}
	if adapter == nil {
		return store.ImportResult{}, fmt.Errorf("source adapter %q is not configured", detail.Session.Agent)
	}
	info, err := os.Stat(detail.Session.SourcePath)
	if err != nil {
		return store.ImportResult{}, fmt.Errorf("stat session source: %w", err)
	}
	imported, err := adapter.Read(ctx, Candidate{
		Agent: detail.Session.Agent, NativeSessionID: detail.Session.NativeSessionID,
		Path: detail.Session.SourcePath, Size: info.Size(), ModTime: info.ModTime().UTC(),
	})
	if err != nil {
		return store.ImportResult{}, err
	}
	if imported.Session.NativeSessionID != detail.Session.NativeSessionID {
		return store.ImportResult{}, fmt.Errorf("source session ID changed from %q to %q", detail.Session.NativeSessionID, imported.Session.NativeSessionID)
	}
	return s.store.ImportSession(ctx, imported.Session, imported.Messages)
}

// NewScanner constructs a transcript scanner.
func NewScanner(database *store.Store, sources ...Source) *Scanner {
	return &Scanner{store: database, sources: sources, scanSlot: make(chan struct{}, 1)}
}

// Scan imports all sources matching agent. The value "all" selects every
// configured source.
func (s *Scanner) Scan(ctx context.Context, agent string) (ScanReport, error) {
	if err := s.acquire(ctx); err != nil {
		return ScanReport{}, err
	}
	defer s.release()
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
			if candidate.Size < 0 || candidate.Size > MaxTranscriptBytes {
				return report, fmt.Errorf("%s transcript %q size %d exceeds %d bytes", adapter.Name(), candidate.Path, candidate.Size, MaxTranscriptBytes)
			}
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

func (s *Scanner) acquire(ctx context.Context) error {
	select {
	case s.scanSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scanner) release() {
	<-s.scanSlot
}

func sourceUnchanged(state store.SourceState, candidate Candidate) bool {
	return state.SourcePath == candidate.Path &&
		state.SourceSize == candidate.Size &&
		state.SourceTime.Equal(candidate.ModTime.UTC().Truncate(time.Nanosecond))
}

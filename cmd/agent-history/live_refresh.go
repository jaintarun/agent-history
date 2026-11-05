package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jaintarun/agent-history/internal/analyze"
	"github.com/jaintarun/agent-history/internal/store"
)

type liveSessionStore interface {
	OpenCmuxSessionIDs(context.Context) ([]string, error)
	NeedsLiveRefocus(context.Context, string, string) (bool, error)
}

type liveSessionScanner interface {
	RefreshIfChanged(context.Context, string) (store.ImportResult, bool, error)
}

type liveAnalysisQueue interface {
	EnqueueLive(context.Context, string, analyze.Options) (<-chan error, error)
}

type liveAnalysisSettings func(context.Context) (analyze.Options, bool, error)

func startLiveRefreshLoop(ctx context.Context, database liveSessionStore, scanner liveSessionScanner, queue liveAnalysisQueue, load liveAnalysisSettings, interval time.Duration, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshLiveSessions(ctx, database, scanner, queue, load, logger)
			}
		}
	}()
	return done
}

func refreshLiveSessions(ctx context.Context, database liveSessionStore, scanner liveSessionScanner, queue liveAnalysisQueue, load liveAnalysisSettings, logger *slog.Logger) {
	options, auto, err := load(ctx)
	if err != nil {
		logLiveRefreshError(logger, "load analysis settings", "", err)
		return
	}
	ids, err := database.OpenCmuxSessionIDs(ctx)
	if err != nil {
		logLiveRefreshError(logger, "list open cmux sessions", "", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		_, changed, err := scanner.RefreshIfChanged(ctx, id)
		if err != nil {
			logLiveRefreshError(logger, "refresh source", id, err)
			continue
		}
		if !auto {
			continue
		}
		if !changed {
			needed, err := database.NeedsLiveRefocus(ctx, id, options.PromptVersion)
			if err != nil {
				logLiveRefreshError(logger, "check live root", id, err)
				continue
			}
			if !needed {
				continue
			}
		}
		if _, err := queue.EnqueueLive(ctx, id, options); err != nil && !errors.Is(err, analyze.ErrAlreadyQueued) {
			logLiveRefreshError(logger, "queue live analysis", id, err)
		}
	}
}

func logLiveRefreshError(logger *slog.Logger, operation, sessionID string, err error) {
	if logger != nil {
		logger.Warn("live session refresh failed", "operation", operation, "session_id", sessionID, "error", err)
	}
}

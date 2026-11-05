# Live-session priority and recency-first metadata implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refresh open cmux agent sessions promptly and replace Agent History's title/overview with recency-first metadata that cmux can mirror.

**Architecture:** A five-minute targeted scan checks source metadata for matched open sessions while the existing full scan discovers new ones. Changed sessions enter a priority lane of the existing serialized worker. Root analysis uses compact topic and tail-leaf summaries with a root-specific cache identity, leaving leaf/topic caches intact.

**Tech Stack:** Go, SQLite/FTS5, embedded web API, existing cmux reconciler.

**Spec:** `docs/superpowers/specs/2026-11-05-live-session-recency-design.md`

## Global Constraints

- Preserve the 15-minute full discovery scan and one-minute cmux reconciliation.
- Five-minute live refresh; no model request for unchanged normalized conversation.
- No new setting, provider, schema rewrite, or cmux Vault integration.
- One serialized model worker; preserve previous visible analysis on failure.
- No real model calls in tests.

## Review Focus

- Source missing or moved since the last full scan: live refresh logs and skips without halting the loop (Task 1).
- Tool-only file append: source metadata may change but normalized messages do not; no analysis call (Task 1).
- Full historical queue: changed live jobs still enter and run first; historical jobs are not discarded (Task 2).
- One topic that changes focus: metadata favors its newest leaf while chapters remain chronological (Task 3).
- Failed generation or cmux manual override: old visible metadata remains, and no override is clobbered (Tasks 3-4).

---

### Task 1: Targeted live import

**Files:** Modify `internal/store/cmux.go`, `internal/source/scanner.go`; test `internal/store/store_test.go`, `internal/source/scanner_test.go`.

**Interfaces:** `(*store.Store).OpenCmuxSessionIDs(ctx context.Context) ([]string, error)`; `(*source.Scanner).RefreshIfChanged(ctx context.Context, sessionID string) (store.ImportResult, bool, error)` returns whether normalized messages changed.

- [ ] Write failing store test: only matched `open=1` IDs return, in stable order; closed rows do not.
- [ ] Run focused store test and confirm red.
- [ ] Implement query; rerun focused test and confirm green.
- [ ] Write failing scanner tests: unchanged stat skips adapter; a tool-only source append imports metadata but returns `changed=false`; a visible append returns `changed=true`; missing source returns an error.
- [ ] Run focused scanner tests and confirm red.
- [ ] Implement stat-aware refresh under the scanner's existing import lock, preserving explicit `Rescan` semantics; rerun tests.
- [ ] Commit this increment.

### Task 2: Live-priority analysis lane

**Files:** Modify `internal/analyze/worker.go`; test `internal/analyze/worker_test.go`.

**Interfaces:** `(*analyze.Worker).EnqueueLive(ctx context.Context, sessionID string, options analyze.Options) (<-chan error, error)`; `Enqueue` stays normal priority.

- [ ] Write failing tests that block the running job, queue normal jobs, then queue a live job and assert the live job executes next; assert deduplication, bounded capacity, one active model call, and eventual normal-job progress.
- [ ] Run focused worker tests and confirm red.
- [ ] Implement a bounded live lane selected before the normal lane after each completion; ensure a full normal queue does not reject a live job or discard a normal one.
- [ ] Run focused worker tests and confirm green.
- [ ] Commit this increment.

### Task 3: Recency-first root metadata

**Files:** Modify `internal/analyze/engine.go` and related types if necessary; test `internal/analyze/engine_test.go`.

**Interfaces:** `Options.LiveRecency bool` selects the root metadata policy for open sessions; node identity differentiates this root policy without changing leaf/topic prompt versions. `Worker.EnqueueLive` passes this option to `Engine.Analyze`.

- [ ] Write failing fake-analyzer tests for multi-topic and one-topic sessions: root request presents latest compact summaries first, bounds older context, emits recent-first title/overview, and never includes raw transcript in a root-only refresh.
- [ ] Run focused engine tests and confirm red.
- [ ] Implement recency root input/prompt and root-only cache identity; unchanged refocus repeats with zero model calls and reusable leaf/topic nodes. Keep chronological topic segments.
- [ ] Write and run failure test: malformed root output leaves prior stored title, overview, and FTS unchanged.
- [ ] Run focused engine tests and confirm green.
- [ ] Commit this increment.

### Task 4: Five-minute scheduling and integration

**Files:** Modify `cmd/agent-history/main.go`, `internal/cmux/reconciler.go` only if necessary; test `cmd/agent-history/main_test.go`, `internal/cmux/reconciler_test.go`.

**Interfaces:** `startLiveRefreshLoop(ctx, database, scanner, worker, optionsLoader, interval, logger) <-chan struct{}`; use `OpenCmuxSessionIDs`, `RefreshIfChanged`, and `EnqueueLive`. Startup refocus queues previously analyzed open sessions once under the new root identity.

- [ ] Write failing loop tests with fake store/scanner/worker: changed sessions enqueue, unchanged sessions do not, per-session failure does not stop the loop, and cancellation exits. Test already-open unchanged sessions get one root refocus but no repeated model request.
- [ ] Run focused command tests and confirm red.
- [ ] Implement loop at five minutes, start/stop beside full scan and cmux reconciler; ensure cmux snapshot is refreshed before first open-session selection.
- [ ] Test shared Agent History API title/overview and existing cmux title/description auto-sync, disabled sync, and manual override behavior.
- [ ] Run focused command/cmux tests and confirm green.
- [ ] Update `docs/DESIGN.md` and README behavior notes; commit this increment.

### Final verification

- [ ] Run `gofmt` on changed Go files, `go test ./...`, `go test -race ./...`, `go vet ./...`, `govulncheck ./...`, and `go build ./cmd/agent-history`.
- [ ] Review `git diff` and verify the running loopback service after restart without invoking a real model from tests.

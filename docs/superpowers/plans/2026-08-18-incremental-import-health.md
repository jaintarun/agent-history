# Incremental Import And Runtime Health Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate SQLite lock stalls during continuous large-session imports, expose scan health, bound WAL retention, and rotate the long-running LaunchAgent log.

**Architecture:** Classify imports as metadata-only, append-only, or rewrite/truncate; only the last path rebuilds a session's complete message and FTS set. Keep four read connections while routing every application-owned mutation through one context-aware writer gate. Record scan state inside `source.Scanner`, expose it through the existing health endpoint, and route LaunchAgent structured logs through a fixed-size rotating writer.

**Tech Stack:** Go 1.24, `database/sql`, modernc SQLite with FTS5/WAL, standard `net/http`, `log/slog`, POSIX shell, macOS LaunchAgents, temporary real SQLite databases, and Go race testing.

**Spec:** `docs/superpowers/specs/2026-08-18-incremental-import-health-design.md`

## Global Constraints

- No schema migration, new setting, analyzer invocation, or search/summary/privacy/cmux behavior change.
- Ordinary appends preserve all existing message and FTS rows and insert only the new suffix.
- Rewrites and truncations retain atomic full replacement and affected-summary invalidation.
- Every public store mutation acquires the writer gate exactly once; transaction helpers never acquire it.
- The four-connection read pool remains available while application writes are serialized.
- `journal_size_limit` is exactly `67108864` bytes on every SQLite connection.
- `GET /api/health` retains top-level `{"status":"ok"}` and adds optional scan state.
- Structured LaunchAgent logs rotate at 8 MiB and retain exactly one backup.
- Tests never invoke Codex or Claude Code and live acceptance does not start a model solely for testing.

---

### Task 1: Append-Only Message And FTS Import

**Files:**
- Modify: `internal/store/import.go`
- Modify: `internal/store/fts.go`
- Modify: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `unchangedPrefix(previous, current []Message) int` and the existing atomic import transaction.
- Produces: `insertMessages(context.Context, *sql.Tx, string, []Message) error`.
- Produces: `insertMessageFTS(context.Context, *sql.Tx, string, string, []Message) error`.
- Invariant: only `rebuildSessionFTS` deletes pre-existing FTS rows, and append-only imports never call it.

- [x] **Step 1: Write failing row-preservation tests**

Add helpers in `internal/store/store_test.go` that query stable ordinary and FTS rowids:

```go
func storedRowIDs(t *testing.T, database *Store, sessionID string) (map[int]int64, map[string]int64) {
	t.Helper()
	messageRows := map[int]int64{}
	rows, err := database.db.Query(`SELECT sequence, rowid FROM messages WHERE session_id = ?`, sessionID)
	if err != nil { t.Fatal(err) }
	defer rows.Close()
	for rows.Next() {
		var sequence int
		var rowID int64
		if err := rows.Scan(&sequence, &rowID); err != nil { t.Fatal(err) }
		messageRows[sequence] = rowID
	}
	ftsRows := map[string]int64{}
	rows, err = database.db.Query(`SELECT document_key, rowid FROM session_fts WHERE session_id = ?`, sessionID)
	if err != nil { t.Fatal(err) }
	defer rows.Close()
	for rows.Next() {
		var key string
		var rowID int64
		if err := rows.Scan(&key, &rowID); err != nil { t.Fatal(err) }
		ftsRows[key] = rowID
	}
	return messageRows, ftsRows
}
```

Add `TestImportSessionAppendPreservesExistingRows`:

```go
func TestImportSessionAppendPreservesExistingRows(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	session := testSession("append-session")
	initial := []Message{
		{Sequence: 0, Timestamp: session.StartedAt, Role: "user", Text: "original alpha"},
		{Sequence: 1, Timestamp: session.LastActiveAt, Role: "assistant", Text: "original beta"},
	}
	if _, err := database.ImportSession(ctx, session, initial); err != nil { t.Fatal(err) }
	messageRows, ftsRows := storedRowIDs(t, database, session.ID)

	appended := append(append([]Message(nil), initial...), Message{
		Sequence: 2, Timestamp: session.LastActiveAt.Add(time.Minute),
		Role: "user", Text: "new zircon suffix",
	})
	session.SourceSize++
	session.SourceMTime = session.SourceMTime.Add(time.Minute)
	session.SourceHash = "appended"
	result, err := database.ImportSession(ctx, session, appended)
	if err != nil { t.Fatal(err) }
	if !result.Changed || result.FirstChangedSequence != 2 { t.Fatalf("result = %#v", result) }

	messageRowsAfter, ftsRowsAfter := storedRowIDs(t, database, session.ID)
	for sequence, rowID := range messageRows {
		if messageRowsAfter[sequence] != rowID { t.Fatalf("message %d rowid changed", sequence) }
	}
	for key, rowID := range ftsRows {
		if ftsRowsAfter[key] != rowID { t.Fatalf("FTS %s rowid changed", key) }
	}
	assertFTSCount(t, database, "zircon", 1)
}
```

Add `TestImportSessionMetadataOnlyPreservesFTSRows` and assert the complete FTS
rowid map stays equal when only source size, hash, mtime, and last-active
metadata change.

Add `TestImportSessionRewriteRemovesStaleText` by changing sequence `0`, then
assert the old FTS term is absent, the replacement term is present, and the old
message/FTS rowids are not required to survive.

- [x] **Step 2: Run the focused tests and verify red**

Run:

```sh
go test ./internal/store -run 'TestImportSession(Append|Metadata|Rewrite)' -count=1
```

Expected: append and metadata tests fail because `ImportSession` currently
deletes and reinserts all message and FTS rows.

- [x] **Step 3: Implement import classification and suffix insertion**

Read the existing session's working directory and existence before `upsertSession`:

```go
func importSessionState(ctx context.Context, tx *sql.Tx, sessionID string) (string, bool, error) {
	var cwd string
	err := tx.QueryRowContext(ctx,
		`SELECT working_directory FROM sessions WHERE id = ?`, sessionID).Scan(&cwd)
	if err == sql.ErrNoRows { return "", false, nil }
	if err != nil { return "", false, fmt.Errorf("read import session state: %w", err) }
	return cwd, true, nil
}
```

Extract the existing insert loop into `insertMessages`, and add the narrow FTS
helper:

```go
func insertMessageFTS(ctx context.Context, tx *sql.Tx, sessionID, cwd string, messages []Message) error {
	for _, message := range messages {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO session_fts(session_id, document_key, document_type, title, body, working_directory)
            VALUES (?, printf('message:%d', ?), 'message', ?, ?, ?)`,
			sessionID, message.Sequence, message.ToolName, message.Text, cwd); err != nil {
			return fmt.Errorf("index message %d: %w", message.Sequence, err)
		}
	}
	return nil
}
```

Use this decision inside the transaction:

```go
appendOnly := existed && firstChanged == len(previous) && len(messages) > len(previous)
switch {
case !changed:
	if previousCWD != session.WorkingDirectory {
		err = rebuildSessionFTS(ctx, tx, session.ID)
	}
case appendOnly:
	err = insertMessages(ctx, tx, session.ID, messages[firstChanged:])
	if err == nil {
		err = insertMessageFTS(ctx, tx, session.ID, session.WorkingDirectory, messages[firstChanged:])
	}
default:
	_, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, session.ID)
	if err == nil { err = insertMessages(ctx, tx, session.ID, messages) }
	if err == nil { err = rebuildSessionFTS(ctx, tx, session.ID) }
}
```

For new sessions, use the full path so the session FTS document is created.
Run the existing summary invalidation and partial-status update for both append
and rewrite paths, but not metadata-only imports.

- [x] **Step 4: Verify focused and store behavior**

Run:

```sh
gofmt -w internal/store/import.go internal/store/fts.go internal/store/store_test.go
go test ./internal/store -count=1
go test ./internal/source -run 'TestCodexScanIsIdempotentAndInvalidatesOnlyChangedSuffix' -count=1
```

Expected: all pass; the source test still proves summary-prefix reuse and
rewrite invalidation.

- [x] **Step 5: Commit**

```sh
git add internal/store/import.go internal/store/fts.go internal/store/store_test.go
git commit -m "perf: append only new session index rows"
```

### Task 2: Context-Aware SQLite Writer Gate And WAL Limit

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/import.go`
- Modify: `internal/store/fts.go`
- Modify: `internal/store/sessions.go`
- Modify: `internal/store/analysis.go`
- Modify: `internal/store/cmux.go`
- Modify: `internal/store/store_test.go`

**Interfaces:**
- Produces: `(*Store).acquireWriter(context.Context) error` and `(*Store).releaseWriter()`.
- Consumes: every public mutation method in `internal/store`.
- Invariant: read-only methods never acquire the writer gate.

- [x] **Step 1: Write failing writer serialization tests**

Add `TestStoreWriterGateSerializesPublicMutations`:

```go
func TestStoreWriterGateSerializesPublicMutations(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	if err := database.acquireWriter(ctx); err != nil { t.Fatal(err) }
	done := make(chan error, 1)
	go func() { done <- database.SetSetting(ctx, "analysis.model", "queued-model") }()
	select {
	case err := <-done:
		t.Fatalf("writer completed while gate held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	database.releaseWriter()
	if err := <-done; err != nil { t.Fatal(err) }
}
```

Add `TestStoreWriterGateHonorsCancellation` by holding the gate, canceling a
second context, calling `SetSetting`, expecting `context.Canceled`, releasing
the gate, and proving the setting was not written.

Extend `TestOpenMigratesEmptyDatabaseAndIsIdempotent` so all four acquired
connections assert:

```go
var limit int64
if err := conn.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&limit); err != nil { t.Fatal(err) }
if limit != 64<<20 { t.Fatalf("journal size limit = %d", limit) }
```

- [x] **Step 2: Run tests and verify red**

Run:

```sh
go test ./internal/store -run 'Test(StoreWriterGate|OpenMigrates)' -count=1
```

Expected: compilation fails because the writer gate does not exist; after adding
only method stubs, cancellation/serialization and WAL-limit assertions fail.

- [x] **Step 3: Add the writer gate and WAL pragma**

Change `Store` and initialization:

```go
type Store struct {
	db         *sql.DB
	writerSlot chan struct{}
}

store := &Store{db: db, writerSlot: make(chan struct{}, 1)}
store.writerSlot <- struct{}{}
```

Add to the DSN pragmas:

```go
query.Add("_pragma", "journal_size_limit(67108864)")
```

Add context-aware ownership:

```go
func (s *Store) acquireWriter(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.writerSlot:
		return nil
	}
}

func (s *Store) releaseWriter() { s.writerSlot <- struct{}{} }
```

- [x] **Step 4: Gate every public mutation exactly once**

At the beginning of each public mutating method, before `BeginTx` or
`ExecContext`, add:

```go
if err := s.acquireWriter(ctx); err != nil { return err }
defer s.releaseWriter()
```

Apply it to `RebuildFTS`, `ImportSession`, `UpsertSession`, `ReplaceMessages`,
`DeleteSession`, `SetSetting`, `SetSettings`, `UpdateSessionTitle`,
`ReplaceAnalysis`, `DeleteAnalysis`, `SetAnalysisStatus`,
`RecoverAnalysisStates`, `PutSummaryNode`, `DeleteSummaryNode`,
`InvalidateSummarySuffix`, `ReplaceCmuxSnapshot`, and `RecordCmuxPush`.

For methods returning `(value, error)`, return the zero value with the acquire
error. Do not gate `migrate`, read methods, `putSummaryNode`,
`invalidateSummarySuffix`, `rebuildSessionFTS`, `insertMessages`, or
`insertMessageFTS` because they are private helpers called under existing
ownership.

- [x] **Step 5: Verify store concurrency**

Run:

```sh
gofmt -w internal/store/*.go
go test ./internal/store -count=1
go test -race ./internal/store -count=1
```

Expected: all pass with no races or `SQLITE_BUSY` errors.

- [x] **Step 6: Commit**

```sh
git add internal/store
git commit -m "fix: serialize sqlite writers and bound wal"
```

### Task 3: Scan Progress In The Health API

**Files:**
- Modify: `internal/source/scanner.go`
- Modify: `internal/source/scanner_test.go`
- Modify: `internal/httpapi/api.go`
- Modify: `internal/httpapi/api_test.go`

**Interfaces:**
- Produces: `source.ScanStatus` and `(*source.Scanner).Status() source.ScanStatus`.
- Produces: optional HTTP-side `scanStatusProvider` interface.
- Preserves: existing `httpapi.Scanner` implementations that only implement `Scan` and `Rescan`.

- [ ] **Step 1: Write failing scanner lifecycle tests**

Use the existing blocking source to add `TestScannerStatusTracksRunningAndSuccess`:

```go
result := make(chan error, 1)
go func() { _, err := scanner.Scan(context.Background(), "codex"); result <- err }()
<-adapter.entered
status := scanner.Status()
if !status.Running || status.StartedAt.IsZero() { t.Fatalf("running status = %#v", status) }
close(adapter.release)
if err := <-result; err != nil { t.Fatal(err) }
status = scanner.Status()
if status.Running || status.FinishedAt.IsZero() || status.LastError != "" {
	t.Fatalf("finished status = %#v", status)
}
```

Add `TestScannerStatusRecordsFailureAndPreservesLastSuccessfulReport`: run one
successful source scan, replace/use a source whose `Discover` returns
`errors.New("discovery failed")`, then assert `LastError` is set while
`LastReport` remains the previous successful report.

- [ ] **Step 2: Run scanner tests and verify red**

Run:

```sh
go test ./internal/source -run 'TestScannerStatus' -count=1
```

Expected: compilation fails because `ScanStatus` and `Status` do not exist.

- [ ] **Step 3: Implement scanner status snapshots**

Add:

```go
type ScanStatus struct {
	Running      bool
	StartedAt    time.Time
	FinishedAt   time.Time
	Duration     time.Duration
	LastReport   ScanReport
	LastError    string
}
```

Add `statusMu sync.RWMutex` and `status ScanStatus` to `Scanner`. Convert `Scan`
to named returns. Immediately after acquiring `scanSlot`, set running state and
defer a finalizer that records duration, finish time, error, and on success the
new report. `Status` returns a value copy while holding `RLock`.

- [ ] **Step 4: Write failing HTTP health tests**

Give `fakeScanner` an optional `status source.ScanStatus` and a `Status` method.
Add `TestHealthIncludesScanStatus` that sets a running status and asserts the
decoded response contains:

```json
{
  "status": "ok",
  "scan": {
    "running": true,
    "started_at": "2026-08-18T15:00:00Z",
    "last_duration_ms": 1200,
    "last_report": {"discovered": 372, "imported": 2, "metadata_only": 1, "skipped": 369}
  }
}
```

Also construct a scanner that implements only the existing interface and prove
the response remains exactly `{"status":"ok"}` apart from JSON whitespace.

- [ ] **Step 5: Implement the optional health projection**

Add:

```go
type scanStatusProvider interface { Status() source.ScanStatus }

type healthResponse struct {
	Status string              `json:"status"`
	Scan   *scanHealthResponse `json:"scan,omitempty"`
}
```

Map `time.Time` values to RFC3339 strings only when nonzero, map duration to
milliseconds, and return the last report and error without querying SQLite.

- [ ] **Step 6: Verify and commit**

Run:

```sh
gofmt -w internal/source/scanner.go internal/source/scanner_test.go internal/httpapi/api.go internal/httpapi/api_test.go
go test ./internal/source ./internal/httpapi -count=1
go test -race ./internal/source ./internal/httpapi -count=1
git add internal/source internal/httpapi
git commit -m "feat: expose scan progress in health api"
```

### Task 4: Rotating LaunchAgent Structured Log

**Files:**
- Create: `cmd/agent-history/rotating_writer.go`
- Create: `cmd/agent-history/rotating_writer_test.go`
- Modify: `cmd/agent-history/main.go`
- Modify: `install-startup.sh`
- Modify: `internal/launch/startup_scripts_test.go`

**Interfaces:**
- Produces: `newRotatingWriter(path string, maxBytes int64) (*rotatingWriter, error)`.
- Produces: `serviceLogWriter(fallback io.Writer) (io.Writer, io.Closer)`.
- Consumes: `AGENT_HISTORY_LOG_PATH` only for the long-running service.

- [ ] **Step 1: Write failing rotation tests**

Add `TestRotatingWriterKeepsOneBoundedBackup` using a 32-byte limit. Write two
24-byte records, close, and assert the first record is in `<path>.1`, the second
is in `<path>`, no `.2` exists, and both files have permission `0600`.

Add `TestRotatingWriterReplacesPreviousBackup` by forcing two rotations and
asserting `.1` contains the immediately previous generation.

Add `TestRotatingWriterSerializesConcurrentWrites` with a limit larger than the
complete test output, eight goroutines writing fixed newline-terminated records,
and an assertion that every complete record exists exactly once.

Add `TestServiceLogWriterUsesConfiguredPath` with `t.Setenv`, write through the
returned writer, close it, and assert the configured file received the text.

- [ ] **Step 2: Run tests and verify red**

Run:

```sh
go test ./cmd/agent-history -run 'Test(RotatingWriter|ServiceLogWriter)' -count=1
```

Expected: compilation fails because the writer functions do not exist.

- [ ] **Step 3: Implement the fixed one-backup writer**

Use this focused shape:

```go
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}
```

`newRotatingWriter` validates a positive maximum, creates the parent directory
with `0700`, opens the active file with `O_CREATE|O_APPEND|O_WRONLY` and `0600`,
and records its size. `Write` locks, rotates before `size+len(p)>maxBytes` when
the active file is nonempty, writes once, and updates size. `rotate` closes the
active file, removes `.1`, renames the active path to `.1`, and opens a fresh
active file. `Close` is idempotent under the mutex.

Add a local `nopWriteCloser` that embeds `io.Writer` and whose `Close` returns
nil. `serviceLogWriter` returns that fallback wrapper when the environment
variable is empty or opening fails; on opening failure it writes one concise
warning to the fallback.

- [ ] **Step 4: Route the service logger and configure LaunchAgent environment**

In `runServe`, before constructing `slog.Logger`, call:

```go
logOutput, logCloser := serviceLogWriter(stderr)
defer logCloser.Close()
logger := slog.New(slog.NewTextHandler(logOutput, nil))
```

In `install-startup.sh`, add the environment key:

```sh
"$plistbuddy" -c "Add :EnvironmentVariables:AGENT_HISTORY_LOG_PATH string $log_dir/agent-history.error.log" "$tmp_plist" >/dev/null
```

Change `StandardErrorPath` to
`$log_dir/agent-history.startup.log`. Launchd then owns only the startup-error
file descriptor and cannot keep writing to a structured log after the
application rotates it.

Extend the startup script test with:

```go
assertPlistValue(t, plistPath, "EnvironmentVariables.AGENT_HISTORY_LOG_PATH",
	filepath.Join(home, "Library", "Logs", "agent-history.error.log"))
assertPlistValue(t, plistPath, "StandardErrorPath",
	filepath.Join(home, "Library", "Logs", "agent-history.startup.log"))
```

- [ ] **Step 5: Verify and commit**

Run:

```sh
gofmt -w cmd/agent-history/rotating_writer.go cmd/agent-history/rotating_writer_test.go cmd/agent-history/main.go internal/launch/startup_scripts_test.go
go test ./cmd/agent-history ./internal/launch -count=1
go test -race ./cmd/agent-history -run 'Test(RotatingWriter|ServiceLogWriter)' -count=1
git add cmd/agent-history install-startup.sh internal/launch/startup_scripts_test.go
git commit -m "feat: rotate launch agent service logs"
```

### Task 5: Documentation, Full Verification, And Live Deployment

**Files:**
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-08-18-incremental-import-health-design.md`
- Modify: `docs/superpowers/plans/2026-08-18-incremental-import-health.md`

**Interfaces:**
- Documents: the additive health response, log locations/rotation, and
  incremental performance behavior.
- Deploys: the committed LaunchAgent at `http://127.0.0.1:54321/`.

- [ ] **Step 1: Update operator documentation**

Add a concise **Runtime Health** README section containing:

```sh
curl http://127.0.0.1:54321/api/health | jq
tail -f ~/Library/Logs/agent-history.error.log
```

State that normal appends update only new message/index rows, scans are reported
under `.scan`, the active log rotates at 8 MiB with one `.1` backup, and cmux,
search, and the UI remain available during background scanning.

Change the spec status from `Approved` to `Implemented` only after all live
acceptance checks pass. Mark plan checkboxes as work completes.

- [ ] **Step 2: Run complete repository verification**

Run:

```sh
gofmt -w $(find cmd internal -name '*.go')
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
govulncheck ./...
go build ./cmd/agent-history
git diff --check
```

Expected: every command exits zero. Do not deploy on a partial pass.

- [ ] **Step 3: Commit implementation documentation**

```sh
git add README.md docs/superpowers/specs/2026-08-18-incremental-import-health-design.md docs/superpowers/plans/2026-08-18-incremental-import-health.md
git commit -m "docs: document runtime health operations"
```

- [ ] **Step 4: Prevent analyzer usage during deployment restart**

Read current settings and save `analysis_auto`. If true, temporarily disable it
through the loopback API before reinstalling:

```sh
curl -fsS -X PUT -H 'Content-Type: application/json' \
  -H 'Origin: http://127.0.0.1:54321' \
  --data '{"analysis_auto":false}' \
  http://127.0.0.1:54321/api/settings
```

This prevents restart recovery from spending model usage on the ten existing
failed sessions. Restore the saved setting after live scan acceptance.

- [ ] **Step 5: Deploy and validate the running service**

Record current lock/500 log counts, then run:

```sh
./install-startup.sh
curl -fsS http://127.0.0.1:54321/api/health | jq
sqlite3 -readonly ~/.local/share/agent-history/history.db 'PRAGMA quick_check(1);'
```

Run one manual scan in the background with the required origin and poll
`/api/health`, `/api/sessions?limit=50`, `/api/sessions/facets`, and
`/api/settings` every second with a 2-second request timeout. Acceptance is:

- every probe returns HTTP 200;
- health reports `scan.running=true` during the scan and a completed result
  afterward;
- no probe exceeds two seconds;
- the manual scan succeeds;
- no new `SQLITE_BUSY`, `database is locked`, or HTTP 500 log entry appears;
- `PRAGMA journal_size_limit` reports `67108864`; and
- the app log and at most one `.1` backup exist within the documented bounds.

Restore `analysis_auto` to its saved value after the manual scan.

- [ ] **Step 6: Final commit/push and public verification**

If live acceptance changes only plan/spec status, commit it:

```sh
git add docs/superpowers/specs/2026-08-18-incremental-import-health-design.md docs/superpowers/plans/2026-08-18-incremental-import-health.md
git commit -m "docs: record performance acceptance"
```

Push `main`, verify the remote SHA equals local `HEAD`, confirm the worktree is
clean, and report the measured live scan/API results rather than only saying the
service is healthy.

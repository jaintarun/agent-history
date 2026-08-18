# Incremental Import And Runtime Health Design

**Date:** 2026-08-18
**Status:** Approved

## Purpose

Keep Agent History responsive while continuously importing very large active
Codex and Claude Code sessions. Ordinary transcript appends must not rewrite
hundreds of thousands of existing SQLite and FTS rows, and concurrent scan,
analysis, and cmux work must not compete as independent SQLite writers.

The running corpus that exposed this problem contains 372 sessions, about
339,000 normalized messages, a 3.0 GB database, and a 633 MB allocated WAL. One
session contains about 80,000 messages. A four-session scan held a write
transaction long enough to produce repeated `SQLITE_BUSY` errors, API requests
as slow as 36 seconds, seven HTTP 500 responses, and an 855-second scan.

This change also makes scan progress visible through the health API, bounds WAL
retention, and rotates the long-running service log.

## Scope

The implementation has five parts:

1. append-only message and FTS updates for normal transcript growth;
2. one context-aware writer gate shared by all application-owned SQLite writes;
3. bounded WAL retention through SQLite connection pragmas;
4. scan progress and last-result data in `GET /api/health`; and
5. size-based rotation for the LaunchAgent's structured application log.

There is no schema migration, no new setting, no analyzer invocation, and no
change to search, summary, privacy, or cmux semantics. Source adapters may still
parse a changed JSONL transcript from the beginning. Incremental file parsing is
deferred unless measurements after this change show that parsing, rather than
database writes, is still a user-visible bottleneck.

## Incremental Import

`Store.ImportSession` continues to compare the stored normalized messages with
the newly normalized source and finds the first changed sequence. It classifies
the import into one of three paths:

- **Metadata only:** the old and new normalized message lists are identical.
  Update session metadata but do not touch message or FTS documents.
- **Append only:** every stored message is an unchanged prefix and the new list
  is longer. Insert only the new message rows, insert only their FTS documents,
  invalidate only affected summary nodes, and mark a current analysis partial.
- **Rewrite or truncate:** any stored message changed or disappeared. Preserve
  the existing conservative behavior: replace the complete message set,
  invalidate the affected summary suffix, and rebuild that session's FTS rows.

The append path must preserve the rowids of existing message FTS documents.
This is the deterministic proof that existing documents were not deleted and
reinserted. Newly appended messages must remain immediately searchable, and a
later rewrite must still remove stale message text from ordinary storage and
FTS.

Session and segment FTS documents remain unchanged during an append. They are
replaced atomically when analysis completes, as they are today. A metadata-only
working-directory change refreshes that session's FTS documents because the
working directory is stored in each FTS row; normal source size, mtime, hash,
and activity changes do not.

## SQLite Writer Coordination

`Store` owns a capacity-one writer gate. Acquiring the gate honors the caller's
context, and release is guaranteed with `defer`. Every public mutation acquires
the gate exactly once before its first write and holds it through commit or
rollback. Private transaction helpers never acquire it, preventing nested
deadlocks.

The gate covers session imports, full FTS rebuilds, settings, analysis state and
replacement, summary-node mutations, cmux snapshots and title provenance, and
session/title deletion or replacement. The existing four-connection pool stays
available for concurrent readers. SQLite's five-second `busy_timeout` remains
as protection against external processes, but normal in-process writers wait on
the context-aware gate instead of racing and returning `SQLITE_BUSY`.

No retry loop is added. Retrying an expensive transaction would repeat work and
hide poor ownership rather than prevent contention.

## WAL Bound

Every pooled connection continues to enable WAL, foreign keys, and
`synchronous=NORMAL`. It additionally sets:

```text
journal_size_limit = 67108864
```

SQLite can reuse WAL space but truncates retained WAL storage to at most 64 MiB
after checkpoints. Automatic checkpointing remains at SQLite's default cadence;
the application does not run blocking truncate checkpoints during requests or
scans.

## Scan Health

`source.Scanner` records an immutable snapshot protected by a small mutex:

- whether a scan is running;
- when the active or last scan started;
- when the last scan finished;
- the last duration in milliseconds;
- the last `ScanReport`; and
- the last error string, cleared by the next successful scan.

The state begins only after the scanner acquires its existing single-scan slot,
so a waiting manual request is not reported as a second active scan. Both
periodic and manual full scans use the same state. Per-session `Rescan` remains
outside this global status.

`GET /api/health` keeps its existing top-level `{"status":"ok"}` contract and
adds a `scan` object when the configured scanner supplies status. Existing fake
or third-party scanner implementations remain valid through an optional status
interface. A health read never starts a scan or writes to SQLite.

## Log Rotation

The LaunchAgent sets `AGENT_HISTORY_LOG_PATH` to
`~/Library/Logs/agent-history.error.log`. When this variable is present, the
server's structured logger uses a small internal rotating writer:

- rotate before a write would take the active file above 8 MiB;
- retain one backup at `<path>.1`;
- create files with user-only permissions;
- serialize writes and rotation with a mutex; and
- fall back to the supplied stderr writer if the log file cannot be opened.

Interactive CLI runs without the environment variable continue logging to
stderr. Launchd captures startup failures in
`~/Library/Logs/agent-history.startup.log`, while ongoing structured service
logs use the rotating writer. Separating these file descriptors prevents
launchd from continuing to write into a file after the application renames it
during rotation. No logging framework or user-facing log settings are added.

## Error Handling

- Cancellation while waiting for the writer gate returns the context error and
  performs no write.
- A failed append or rewrite rolls back session metadata, messages, FTS, and
  summary invalidation together.
- A failed scan records its duration and error, while preserving the last
  successful report for diagnostics.
- Failure to rotate or reopen a log returns the write error to `slog`; it does
  not terminate the service.
- The health endpoint remains available while scans and analysis are running.

## Testing

Development follows red-green-refactor tests against temporary real SQLite
databases.

Store tests prove:

- an append preserves existing message and FTS rowids;
- only new messages are added and searchable;
- metadata-only imports preserve every FTS rowid;
- rewrites and truncations remove stale rows and text;
- a held writer gate blocks another public writer until release;
- a canceled writer wait returns without mutation; and
- every pooled connection reports the 64 MiB journal size limit.

Source and HTTP tests prove scan status transitions through running, success,
and failure, and that the health JSON remains backward compatible.

Logging and installer tests prove rotation size, single-backup replacement,
permissions, concurrent writes, and the LaunchAgent environment path.

Repository verification remains:

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
go build ./cmd/agent-history
```

Live acceptance reinstalls the LaunchAgent, confirms the database quick check,
runs a scan while repeatedly requesting health, sessions, facets, and settings,
and verifies no new `SQLITE_BUSY` or HTTP 500 log entries. No real analyzer is
started solely for testing.

## Completion Criteria

The work is complete when:

- ordinary appends do not delete or reinsert stored message/FTS prefixes;
- rewrites retain the current atomic correctness behavior;
- application-owned writers cannot collide inside SQLite;
- WAL retention is configured to 64 MiB;
- health reports active and last scan state;
- the LaunchAgent log rotates at 8 MiB with one backup;
- all repository verification commands pass; and
- the deployed service remains responsive and records no lock or 500 errors
  during live scan acceptance.

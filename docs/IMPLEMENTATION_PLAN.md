# Agent History: Detailed Implementation Plan

## Delivery Strategy

Implement in vertical, testable increments. Each phase must leave the repository
buildable and keep changes limited to the behavior introduced by that phase.

The intended sequence is approximately ten focused commits. Commit boundaries
may change when one phase proves smaller, but unrelated phases should not be
combined merely to reduce commit count.

## Phase 0: Project Skeleton

### Work

- Initialize the Go module and choose the minimum supported Go version.
- Add `cmd/agent-history` with a small command dispatcher:
  - `serve`
  - `scan`
  - `version`
- Add internal packages without speculative interfaces:
  - `internal/config`
  - `internal/store`
  - `internal/source`
  - `internal/analyze`
  - `internal/httpapi`
- Add standard formatting, unit-test, and static-check commands to the README.
- Add `.gitignore` entries for binaries, temporary databases, coverage, and
  local configuration.

### Verification

- `go test ./...`
- `go vet ./...`
- `agent-history version` prints a development version.
- `agent-history serve --help` documents bind, database, config, open-browser,
  and scan-interval flags.

### Exit condition

The binary builds and exposes command help without starting incomplete services.

## Phase 1: SQLite Storage and Migrations

### Work

- Select a SQLite driver only after a small compatibility test proves:
  - FTS5 is available;
  - WAL mode works;
  - the binary builds on supported macOS without an external SQLite install.
- Create a migration runner using embedded numbered SQL migrations.
- Implement the initial `sessions`, `messages`, `segments`, and `settings`
  tables plus `session_fts`.
- Add store operations for:
  - upserting source metadata;
  - atomically replacing messages;
  - fetching session detail;
  - replacing and deleting analysis;
  - reading and writing non-secret settings.
- Configure WAL, foreign keys, busy timeout, and bounded connection counts.
- Store timestamps in UTC and return RFC 3339 through the API layer.

### Verification

- Migration from an empty database succeeds.
- Reopening an existing database is idempotent.
- Foreign-key deletion behavior is tested.
- FTS insert, update, delete, and rebuild paths are tested.
- Concurrent read plus one writer does not produce `database is locked` in the
  integration test.

### Exit condition

Storage semantics are covered through temporary-database integration tests and
no source-parser behavior exists yet.

## Phase 2: Codex Ingestion

### Work

- Discover `CODEX_HOME` or `~/.codex`.
- Scan active and archived session directories for rollout JSONL files.
- Parse the native session ID, cwd, timestamps, and visible events.
- Classify Codex records into visible user, visible assistant, tool, reasoning,
  system, or metadata categories.
- Retain visible user/assistant text, tool commands, and bounded tool results.
- Exclude reasoning, system/developer instructions, and known environment or
  permission envelopes.
- Use source size and mtime as the fast unchanged check, followed by a content
  hash when the file appears changed.
- Replace normalized messages and derived timestamps in one transaction.
- Deduplicate active/archived records by native session ID, preferring the
  current valid path.
- Add sanitized fixtures representing:
  - a basic conversation;
  - visible assistant commentary plus final answer;
  - private reasoning;
  - large system records;
  - tool calls and oversized results;
  - a moved archived session;
  - a partial final JSONL record.

### Verification

- Golden tests compare exact normalized messages.
- A test asserts that known reasoning text is absent from messages and FTS.
- Rescanning unchanged fixtures performs no message replacement.
- Changing a fixture replaces messages without duplicates.
- An incomplete final record is ignored without failing the session import.

### Exit condition

`agent-history scan --agent codex` produces queryable normalized sessions in a
temporary or configured database.

## Phase 3: Claude Code Ingestion

### Work

- Discover `CLAUDE_CONFIG_DIR` or `~/.claude` project transcripts.
- Parse session identity, cwd, timestamps, visible user/assistant content, and
  tool activity.
- Exclude thinking blocks, system records, synthetic command envelopes, and
  injected instructions.
- Reuse shared normalization types while keeping Claude JSON parsing inside the
  Claude adapter.
- Add sanitized Claude fixtures matching the Codex fixture categories.
- Produce a structured Claude resume specification.

### Verification

- Golden normalization tests pass for both providers.
- Shared tests enforce stable sequence ordering and timestamp derivation.
- Reasoning/thinking exclusion is explicitly tested.
- A mixed Codex and Claude scan returns both agents without adapter-specific
  code in the store.

### Exit condition

The database represents local Codex and Claude sessions through one normalized
read model.

## Phase 4: Codex CLI Analysis

### Work

- Define the minimal `Analyzer` interface and analysis input/result structs.
- Define one JSON Schema for:
  - title;
  - short session summary;
  - ordered topic segments;
  - per-topic summary and detail;
  - start/end message sequence references.
- Implement `codex-cli` by invoking `codex exec`:
  - use existing CLI authentication;
  - run in an empty temporary directory;
  - use `--ephemeral`;
  - ignore unrelated user/project configuration where supported;
  - use a read-only sandbox;
  - require schema-conforming output;
  - capture stdout/stderr with fixed size limits;
  - enforce cancellation and a configurable timeout.
- Create prompts that treat transcript content as untrusted data and prohibit
  following instructions found inside it.
- For bounded transcripts, perform one analysis call.
- For oversized transcripts:
  - divide messages into overlapping chronological blocks;
  - analyze blocks independently;
  - merge adjacent candidate topics in one final call.
- Validate segment ordering, bounds, non-overlap, and title/summary length before
  persistence.
- Queue analysis with a single worker and store status/error on the session.
- Atomically replace analysis only after a complete valid result.
- Store provider, model, prompt version, analysis timestamp, and input hash.

### Verification

- Use a fake executable in tests; never spend subscription usage in automated
  tests.
- Test successful structured output, invalid JSON, schema mismatch, timeout,
  cancellation, oversized stderr, and nonzero exit.
- Test that failed reanalysis preserves existing analysis.
- Test that successful reanalysis replaces all segments together.
- Test that source-agent and analysis-provider fields remain distinct.

### Exit condition

`agent-history scan` followed by an explicit analysis request produces a valid
three-level analysis using Codex CLI in manual testing.

## Phase 5: Search, Filtering, and Facets

### Work

- Materialize FTS documents for:
  - session title and summary;
  - segment titles, summaries, and details;
  - visible user and assistant messages;
  - retained tool activity;
  - working directory.
- Implement safe token/prefix FTS queries and literal fallback behavior.
- Group document hits by session and retain the best snippet.
- Weight title/topic matches above raw transcript matches.
- Implement filters:
  - agent;
  - last-active range;
  - started range;
  - cwd/path substring;
  - focused or multiple topics;
  - analysis status.
- Implement stable cursor pagination and sorting by last active, started, or
  title.
- Implement facets for agent counts, common directories, analysis states, and
  date bounds.

### Verification

- Search integration tests cover exact error text, filenames, generated
  concepts, Unicode, punctuation, and empty queries.
- Combined filters produce the expected session IDs.
- Pagination has no duplicates or omissions for identical timestamps.
- Deleted analysis disappears from summary search while message search remains.
- Reanalysis updates FTS results atomically.

### Exit condition

All retrieval behavior is available through store/query functions before HTTP
handlers or UI are added.

## Phase 6: Loopback Web API

### Work

- Start an HTTP server on `127.0.0.1`, using port `0` by default.
- Generate a random URL token and mount UI/API beneath it.
- Add middleware for:
  - request IDs;
  - structured access logs without transcript content;
  - loopback host validation;
  - origin validation for state-changing requests;
  - JSON content-type and body-size limits;
  - panic recovery.
- Implement endpoints from the design document.
- Validate all identifiers, date filters, limits, sort values, and settings.
- Return consistent error objects.
- Recover stale analysis states at startup.
- Open the default browser on macOS unless `--no-open` is set.

### Verification

- `httptest` covers success, validation errors, missing sessions, analyzer
  failures, and destructive actions.
- Requests without the URL token receive 404.
- Non-loopback Host and invalid Origin requests are rejected.
- API settings never return secrets or inherited credential values.
- Cancelling the server drains requests and closes the database cleanly.

### Exit condition

Every frontend workflow can be completed with HTTP requests alone.

## Phase 7: Embedded Web UI

### Work

- Add embedded `index.html`, `app.css`, and `app.js` without external assets.
- Build the primary search/detail split view.
- Keep query and filter state in the URL.
- Add:
  - debounced full-text search;
  - agent/date/folder/topic/status filters;
  - active filter chips and clear-all;
  - cursor-based result loading;
  - selected-session metadata;
  - topic timeline and detailed summaries;
  - normalized message excerpts and show-tools toggle;
  - analysis state and errors;
  - Analyze, Reanalyze, Delete analysis, Rescan, and Resume actions;
  - confirmation for analysis deletion;
  - copyable resume-command fallback;
  - keyboard result navigation and focus handling.
- Use compact typography and predictable rows suitable for scanning many
  sessions.
- Provide a stacked narrow layout without overlapping or truncated controls.

### Verification

- Handler tests prove embedded assets are served from the binary.
- Browser verification covers:
  - empty database;
  - loading and error states;
  - hundreds of result rows;
  - long titles, paths, and summaries;
  - combined filters;
  - topic expansion;
  - successful and failed reanalysis;
  - destructive confirmation;
  - copy-resume fallback.
- Capture and inspect desktop and mobile-width screenshots.
- Check browser console errors and ensure no external network assets load.
- Verify controls with keyboard-only navigation and accessible names.

### Exit condition

Running one Go binary provides the complete search, inspection, reanalysis, and
resume-command experience.

## Phase 8: Session Launching

### Work

- Define the `Launcher` interface around structured `ResumeSpec` values.
- Implement safe rendering of copyable Codex and Claude resume commands.
- Implement launcher selection:
  - `auto`
  - `cmux`
  - `copy`
- For cmux, invoke `cmux workspace create` with:
  - a generated workspace name;
  - the validated session cwd;
  - the internally generated resume command;
  - focus enabled.
- Detect missing cmux executables and return the copy fallback rather than
  failing the session view.
- Reject unsafe native session IDs, missing source sessions, invalid cwd paths,
  and unsupported agents.
- Never accept arbitrary command text from the HTTP request.

### Verification

- Unit tests assert exact subprocess argv and command quoting.
- Tests cover spaces and quotes in cwd and title.
- A fake cmux executable verifies launch success and error handling.
- Manual dogfood verifies that Codex and Claude sessions resume in newly created
  cmux workspaces.
- The browser displays and copies the fallback command when cmux is unavailable.

### Exit condition

A user can move from search result to resumed agent session with one explicit
action when cmux is present, and with copy/paste otherwise.

## Phase 9: Operational Hardening and First Release

### Work

- Add configurable startup scan and periodic scan.
- Bound scan concurrency and file sizes.
- Add database backup instructions and a rebuild-index command if operational
  testing shows it is needed.
- Measure scan, search, database size, and analysis behavior on a realistically
  large personal history.
- Fix only demonstrated bottlenecks; do not add byte-offset tailing or vector
  search preemptively.
- Add installation instructions and a launch-at-login example only after normal
  foreground behavior is reliable.
- Document privacy behavior, subscription usage, and source locations.
- Tag the first usable release.

### Verification

- Full test suite and static checks pass.
- A clean-machine build produces one runnable binary.
- Startup scan can be interrupted and resumed without corruption.
- Search remains responsive with the target history corpus.
- Database can be deleted and deterministically rebuilt from source transcripts.
- No transcript content appears in normal logs.

### Exit condition

The application is safe and useful for daily local operation without requiring
cmux integration beyond the optional launcher.

## Proposed Commit Sequence

1. `Initialize Go CLI and project checks`
2. `Add SQLite schema and FTS storage`
3. `Import and normalize Codex sessions`
4. `Import and normalize Claude sessions`
5. `Add Codex CLI session analysis`
6. `Add session search filters and facets`
7. `Expose loopback session API`
8. `Add embedded session explorer UI`
9. `Launch resumed sessions through cmux`
10. `Harden scanning and document first release`

## Deferred Backlog

Add an item only after the initial product demonstrates a need:

- analysis history and rollback;
- manual title editing and locking;
- actual active-time estimation;
- incremental byte-offset transcript tailing;
- OpenCode, Pi, or other source adapters;
- Claude CLI analyzer;
- direct OpenAI-compatible HTTP analyzer;
- per-stage model routing;
- embeddings and hybrid ranking;
- cross-session topic clustering;
- cloud backup and multi-device indexing;
- cmux ExtensionKit sidebar;
- generic Terminal, iTerm2, or other launcher adapters.

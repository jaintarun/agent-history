# cmux Session Status and Title Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show exact cmux session status and title differences in Agent History, support guarded manual and automatic title synchronization, and filter sessions by whether they are open in cmux.

**Architecture:** A focused `internal/cmux` package reads cmux's documented hook mappings and newline-delimited Unix-socket API. A serialized reconciler persists live cmux observations and Agent History's last-written title provenance in SQLite, while the existing HTTP API and embedded web UI expose comparison, filtering, settings, refresh, and explicit push behavior.

**Tech Stack:** Go 1.24+, standard-library Unix sockets and JSON, SQLite/FTS5 via `modernc.org/sqlite`, `net/http`, embedded HTML/CSS/JavaScript, macOS cmux 0.64+.

## Global Constraints

- Match sessions only by `(agent, native_session_id)` from cmux hook state; never infer from title, directory, or time.
- Automatic sync never overwrites an independently changed cmux workspace or tab title.
- Manual push may overwrite because it is an explicit user action.
- A cmux failure must not prevent Agent History startup, scanning, analysis, search, or browsing.
- Do not modify cmux Vault, Codex state databases, Claude transcripts, or cmux hook files.
- Keep cmux's built-in AI auto-naming disabled.
- Preserve loopback HTTP, origin validation, parameterized SQL, transactional state replacement, and the existing no-secrets/no-transcript logging rules.
- Add no external Go or browser dependencies.

## File Structure

- Create `internal/store/migrations/002_cmux_state.sql`: persisted live state and last-pushed provenance.
- Create `internal/store/cmux.go`: transactional cmux state replacement, lookup, push provenance, and availability state.
- Modify `internal/store/types.go`, `internal/store/search.go`, `internal/store/sessions.go`: cmux DTO types, filter, and session lookup by native identity.
- Create `internal/cmux/client.go`: socket discovery, JSON RPC, capabilities, workspace/surface list, and rename operations.
- Create `internal/cmux/hooks.go`: documented Claude/Codex hook parsing and normalized mappings.
- Create `internal/cmux/reconciler.go`: serialized refresh, exact matching, automatic-sync policy, and manual push.
- Create `internal/cmux/*_test.go`: fake Unix socket, hook fixtures, and policy tests.
- Modify `cmd/agent-history/main.go` and tests: construct and run the reconciler without making cmux startup-critical.
- Modify `internal/httpapi/api.go` and tests: cmux API boundary, filter, settings, status fields, refresh, and manual push endpoint.
- Modify `internal/httpapi/assets/index.html`, `app.js`, `app.css`, and tests: filter, badges, title comparison, setting, and push control.
- Modify `README.md`, `install-startup.sh`, and startup tests only where needed to document and verify `allowAll` deployment.

---

### Task 1: Persist cmux State and Filter Sessions

**Files:**
- Create: `internal/store/migrations/002_cmux_state.sql`
- Create: `internal/store/cmux.go`
- Modify: `internal/store/types.go`
- Modify: `internal/store/search.go`
- Modify: `internal/store/sessions.go`
- Test: `internal/store/store_test.go`
- Test: `internal/store/search_test.go`

**Interfaces:**
- Produces: `type CmuxSessionState`, `type CmuxAvailability`, `ReplaceCmuxSnapshot(ctx, availability, states) error`, `CmuxState(ctx, sessionID) (CmuxSessionState, bool, error)`, `RecordCmuxPush(ctx, sessionID, workspaceTitle, surfaceTitle string) error`, `SessionsByNativeIdentity(ctx) ([]SessionIdentity, error)`.
- Extends: `SearchQuery.Cmux` with accepted values `""`, `"open"`, and `"closed"`.

- [ ] **Step 1: Write failing migration/state tests**

Add real-SQLite tests that import two sessions, replace a snapshot, assert one open state, record last-pushed workspace/surface titles, replace with an empty successful snapshot, and confirm the observation is retained but `Open` becomes false.

```go
func TestReplaceCmuxSnapshotTracksOpenStateAndPushProvenance(t *testing.T) {
    db := openTestStore(t)
    // Insert session, replace snapshot, record push, then replace with no open states.
    // Assert IDs/titles/lifecycle/timestamps remain valid and Open is false.
}
```

- [ ] **Step 2: Run the focused test and verify RED**

Run: `go test ./internal/store -run 'TestReplaceCmux|TestSearchSessionsFiltersCmux'`

Expected: build failure because cmux store types and methods do not exist.

- [ ] **Step 3: Add the migration and minimal store methods**

Use a foreign-keyed table with `open`, workspace/surface IDs and titles, lifecycle, `workspace_has_custom_title`, `observed_at`, `last_pushed_workspace_title`, `last_pushed_surface_title`, and push timestamps. Replace successful observations in one transaction; retain rows to preserve provenance.

```sql
CREATE TABLE cmux_session_state (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    open INTEGER NOT NULL DEFAULT 0 CHECK (open IN (0, 1)),
    workspace_id TEXT NOT NULL DEFAULT '',
    surface_id TEXT NOT NULL DEFAULT '',
    workspace_title TEXT NOT NULL DEFAULT '',
    surface_title TEXT NOT NULL DEFAULT '',
    workspace_has_custom_title INTEGER NOT NULL DEFAULT 0 CHECK (workspace_has_custom_title IN (0, 1)),
    lifecycle TEXT NOT NULL DEFAULT 'unknown',
    observed_at TEXT NOT NULL,
    last_pushed_workspace_title TEXT,
    last_pushed_surface_title TEXT,
    last_pushed_at TEXT
);
```

- [ ] **Step 4: Add and test the cmux search filter**

Add `Cmux string` to `SearchQuery`, validate only `open|closed`, and filter with `EXISTS`/`NOT EXISTS` against `cmux_session_state.open = 1`. Verify combined filtering and pagination continue returning the expected IDs.

- [ ] **Step 5: Run store verification and commit**

Run: `gofmt -w internal/store/*.go && go test ./internal/store`

Expected: PASS.

Commit: `git commit -am 'feat: persist cmux session state'` after adding new files.

### Task 2: Read cmux API and Hook Mappings

**Files:**
- Create: `internal/cmux/client.go`
- Create: `internal/cmux/client_test.go`
- Create: `internal/cmux/hooks.go`
- Create: `internal/cmux/hooks_test.go`

**Interfaces:**
- Produces: `Client.Capabilities(ctx) (Capabilities, error)`, `Client.Workspaces(ctx) ([]Workspace, error)`, `Client.Surfaces(ctx, workspaceID) ([]Surface, error)`, `Client.RenameWorkspace(ctx, id, title) error`, and `Client.RenameSurface(ctx, workspaceID, surfaceID, title) error`.
- Produces: `LoadHookMappings(home string) ([]HookMapping, []error)` with `Agent`, `NativeSessionID`, `WorkspaceID`, `SurfaceID`, `Lifecycle`, and `UpdatedAt`.

- [ ] **Step 1: Write failing socket-protocol tests**

Start a temporary Unix listener that reads one newline-terminated request and returns a v2 response. Assert request methods and parameters for capabilities, listing, and renames; also test timeout, malformed response, `ok:false`, and unavailable socket behavior.

```go
func TestClientRenameSurfaceUsesExactMappedIDs(t *testing.T) {
    // Expect method tab.action and params action=rename, workspace_id,
    // surface_id, and title. Return {"ok":true,"result":{}}.
}
```

- [ ] **Step 2: Verify socket tests fail for the missing client**

Run: `go test ./internal/cmux -run TestClient`

Expected: build failure because `Client` is undefined.

- [ ] **Step 3: Implement the minimal socket client**

Use `net.Dialer.DialContext("unix", path)`, deadlines, monotonically increasing request IDs, `json.Encoder`, and one `bufio.Reader.ReadBytes('\n')`. Resolve the socket from `CMUX_SOCKET_PATH`, `~/.local/state/cmux/cmux-<uid>.sock`, then `/tmp/cmux.sock`. Validate response ID, `ok`, and result shape.

- [ ] **Step 4: Write failing hook parsing tests**

Use temporary `claude-hook-sessions.json` and `codex-hook-sessions.json` fixtures. Assert valid records normalize lifecycle, malformed one-agent data does not discard the other agent, and duplicate native IDs choose the most recently updated valid mapping deterministically.

- [ ] **Step 5: Implement documented hook parsing and verify**

Decode only the documented `sessions` object, validate nonempty IDs, normalize lifecycle to `running|idle|needsInput|unknown`, and return diagnostics without transcript or prompt content.

Run: `gofmt -w internal/cmux/*.go && go test ./internal/cmux`

Expected: PASS.

Commit: `git add internal/cmux && git commit -m 'feat: read cmux sessions and titles'`.

### Task 3: Reconcile and Safely Synchronize Titles

**Files:**
- Create: `internal/cmux/reconciler.go`
- Create: `internal/cmux/reconciler_test.go`
- Modify: `cmd/agent-history/main.go`
- Modify: `cmd/agent-history/main_test.go`

**Interfaces:**
- Consumes: store cmux state/provenance methods and cmux client/hook mappings.
- Produces: `Reconciler.Refresh(ctx) error`, `Reconciler.PushTitle(ctx, sessionID) error`, `Reconciler.Start(ctx, 60*time.Second) <-chan struct{}`, and `Reconciler.Status() Status`.

- [ ] **Step 1: Write failing exact-match and stale-state tests**

Use a fake cmux API and real temporary SQLite. Verify only exact `(agent, native ID)` matches become open; missing workspace/surface mappings become closed; lifecycle and both titles are stored.

- [ ] **Step 2: Write failing automatic-sync policy tests**

Cover these table cases:

```text
setting off                              -> no writes
analysis current + workspace unowned    -> rename workspace
workspace equals last pushed workspace  -> update workspace
workspace differs from last pushed      -> preserve and mark different
tab equals last pushed tab               -> update tab
tab has no last-pushed provenance        -> preserve tab
analysis partial/empty                   -> no writes
multiple mapped sessions in workspace    -> no automatic writes
```

- [ ] **Step 3: Verify reconciler tests fail**

Run: `go test ./internal/cmux -run 'TestReconciler|TestAutomaticSync|TestManualPush'`

Expected: build failure because `Reconciler` is undefined.

- [ ] **Step 4: Implement serialized reconciliation and policy**

Guard refresh and push with one mutex. Check capabilities for `allowAll`, load current workspaces/surfaces and hooks, join with `SessionsByNativeIdentity`, replace the store snapshot, then evaluate the pure sync policy. Refresh once after writes so returned comparison state reflects cmux.

- [ ] **Step 5: Implement and test explicit push**

Require an open mapping and nonempty Agent History title. Rename the exact surface; rename the workspace only when it contains one mapped session. Record each successful target separately. On partial failure, refresh and return an error without claiming full synchronization.

- [ ] **Step 6: Wire a noncritical 60-second loop into serve**

Construct the reconciler after the store opens, start it alongside the scan loop, and stop it during shutdown. Log only session IDs, method names, and concise errors. A missing cmux socket logs availability at debug/warn level but does not fail `serve`.

- [ ] **Step 7: Verify and commit**

Run: `gofmt -w internal/cmux/*.go cmd/agent-history/*.go && go test ./internal/cmux ./cmd/agent-history`

Expected: PASS.

Commit: `git add internal/cmux cmd/agent-history && git commit -m 'feat: reconcile cmux title state'`.

### Task 4: Expose cmux State and Commands Through HTTP

**Files:**
- Modify: `internal/httpapi/api.go`
- Modify: `internal/httpapi/api_test.go`
- Modify: `internal/httpapi/server_test.go`
- Modify: `cmd/agent-history/main.go`

**Interfaces:**
- Adds `Cmux` dependency to `httpapi.Config` behind an interface with `Refresh`, `PushTitle`, and `Status`.
- Adds `POST /api/cmux/refresh`, `POST /api/sessions/{id}/cmux-title`, `cmux=open|closed`, nested session `cmux`, and setting `cmux_title_sync`.

- [ ] **Step 1: Write failing API tests**

Extend the fake integration boundary and assert:

- list/detail JSON includes open state, lifecycle, titles, and comparison;
- `cmux=open|closed` validates and filters;
- manual push calls the reconciler for the requested stored session;
- refresh invokes reconciliation;
- disabled automatic sync persists while reads/manual push remain available; and
- unavailable cmux is represented without leaking socket paths.

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/httpapi -run 'TestCmux|TestSettings'`

Expected: FAIL because routes and DTO fields do not exist.

- [ ] **Step 3: Implement minimal routes, DTOs, and settings**

Use existing JSON/error/origin middleware. Return `202` only for queued work; synchronous cmux refresh/push returns `200`. Validate empty JSON bodies and existing session IDs. Persist `cmux.title_sync` in the existing settings table and never expose a socket path.

- [ ] **Step 4: Verify HTTP package and commit**

Run: `gofmt -w internal/httpapi/*.go cmd/agent-history/*.go && go test ./internal/httpapi ./cmd/agent-history`

Expected: PASS.

Commit: `git add internal/httpapi cmd/agent-history && git commit -m 'feat: expose cmux title synchronization API'`.

### Task 5: Add Web Controls, Deploy, and Verify

**Files:**
- Modify: `internal/httpapi/assets/index.html`
- Modify: `internal/httpapi/assets/app.js`
- Modify: `internal/httpapi/assets/app.css`
- Modify: `internal/httpapi/api_test.go`
- Modify: `README.md`
- Modify: `install-startup.sh` only if startup verification requires it
- Modify: `internal/launch/startup_scripts_test.go` only if installer behavior changes

**Interfaces:**
- Consumes: nested `session.cmux`, `cmux` query parameter, cmux title endpoint, refresh endpoint, and `cmux_title_sync` setting.

- [ ] **Step 1: Write failing embedded-web contract tests**

Require `cmux-filter`, settings toggle, row badge rendering, comparison copy, and the manual `/cmux-title` workflow. Require the filter in URL restoration and request parameter construction.

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/httpapi -run 'TestWebApplicationIncludesCoreWorkflows'`

Expected: FAIL with missing cmux controls and workflows.

- [ ] **Step 3: Implement the filter and session indicators**

Add the compact select beside existing filters. Preserve it through `filterIDs`, URL params, chips, automatic refresh, and selection. Add a stable-size cmux lifecycle badge to open rows without changing row height.

- [ ] **Step 4: Implement comparison, push, and settings UI**

Render a small unframed comparison band in session detail. Show current target title and status; show the explicit button only when open, different, and Agent History has a title. Add the auto-sync checkbox and cmux connection status to Settings.

- [ ] **Step 5: Run code and repository verification**

Run:

```sh
gofmt -w $(git diff --name-only -- '*.go')
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
go build ./cmd/agent-history
```

Expected: all commands PASS.

- [ ] **Step 6: Enable and verify cmux `allowAll`**

Set the persisted cmux user preference to `allowAll`, verify the live `cmux capabilities --json` reports `access_mode: allowAll`, and ensure cmux's AI auto-naming remains disabled. Do not edit hook files.

- [ ] **Step 7: Deploy the LaunchAgent and perform live acceptance**

Run `./install-startup.sh`, wait for `http://127.0.0.1:54321/api/health`, and inspect logs for startup errors. Verify open/closed filtering, lifecycle, differing titles, manual push, automatic-sync protection, and 60-second refresh in desktop and narrow browser viewports.

- [ ] **Step 8: Document behavior and commit**

Document the cmux prerequisite, sync policy, and troubleshooting in `README.md`.

Commit: `git add internal/httpapi README.md install-startup.sh internal/launch/startup_scripts_test.go && git commit -m 'feat: add cmux title sync controls'`.

- [ ] **Step 9: Review and publish**

Inspect `git diff origin/main...HEAD`, confirm no private titles, transcript content, socket paths, generated binaries, or unrelated changes are committed, then push `main` to `origin` after all live checks pass.

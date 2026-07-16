# cmux Agent Activity Colors and Web Auto-Refresh Control Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Color exact open cmux AI-agent workspaces by transcript recency and let each browser disable all scheduled page refreshes while retaining manual refresh.

**Architecture:** Extend the existing bounded cmux client with `custom_color` and `workspace.action set_color`, then add an always-on color policy to the serialized reconciler using imported `last_active_at`. Keep the web preference entirely client-side in `localStorage`; it gates the existing 60-second timer and selected-session poll without changing server jobs or HTTP settings.

**Tech Stack:** Go 1.24, SQLite, cmux v2 newline-delimited JSON socket API, embedded HTML/CSS/vanilla JavaScript, Go `testing`/`httptest`, browser-harness for live UI verification.

## Global Constraints

- Color only exact open Claude or Codex mappings; never infer identity from titles, paths, processes, or timestamps.
- Use `sessions.last_active_at`, never hook `updatedAt` or lifecycle transitions, as the activity clock.
- Use fixed thresholds and colors: age `<= 1h` Green `#196F3D`; age `> 1h` and `< 5h` Orange `#A04000`; age `>= 5h` Red `#C0392B`.
- For multiple mapped agents in one workspace, use the greatest `last_active_at`.
- Agent History owns the color of a matched agent workspace and may replace a manually selected color.
- Never modify or clear a workspace without an exact open agent mapping.
- Auto refresh defaults on, is stored only in browser `localStorage`, and controls every scheduled page fetch but no server-side job.
- Manual Refresh must work while auto refresh is off and preserve current filters and session selection.
- No database migration, new HTTP endpoint, new dependency, real model call, or transcript-content logging.
- Preserve loopback-only HTTP, origin validation, bounded cmux deadlines, and sanitized browser error responses.

---

## File Structure

- Modify `internal/cmux/client.go`: decode live workspace color and send one exact color mutation.
- Modify `internal/cmux/client_test.go`: socket-contract coverage for color reads and writes.
- Modify `internal/cmux/reconciler.go`: pure age-to-color policy, workspace aggregation, and serialized writes.
- Modify `internal/cmux/reconciler_test.go`: activity boundaries, shared workspace selection, eligibility, idempotence, ownership, and partial failures.
- Modify `internal/httpapi/assets/index.html`: add the browser-local Auto refresh checkbox and bump embedded asset query versions.
- Modify `internal/httpapi/assets/app.js`: load/save preference and gate both timers.
- Modify `internal/httpapi/assets/app.css`: stable compact checkbox layout on desktop and narrow widths.
- Modify `internal/httpapi/api_test.go`: embedded UI contract and cache-version assertions.
- Modify `README.md`: document activity colors and browser-local refresh behavior.
- Modify `run-local.sh`: bump the deployed binary version to `v0.1.8`.

---

### Task 1: Add cmux Workspace Color Protocol Support

**Files:**
- Modify: `internal/cmux/client.go:34-101`
- Test: `internal/cmux/client_test.go:14-57`

**Interfaces:**
- Consumes: existing `Client.call(ctx, method, params, destination) error`.
- Produces: `Workspace.CustomColor string` and `Client.SetWorkspaceColor(context.Context, string, string) error`.

- [ ] **Step 1: Write failing workspace color decoding and mutation tests**

Update the workspace fixture and assertion in `TestClientListsCapabilitiesWorkspacesAndSurfaces`:

```go
{method: "workspace.list", result: map[string]any{"workspaces": []map[string]any{{
    "id": "w1", "title": "Workspace", "custom_title": "Workspace",
    "has_custom_title": true, "custom_color": "#196F3D",
}}}},
```

```go
if len(workspaces) != 1 || workspaces[0].ID != "w1" ||
    !workspaces[0].HasCustomTitle || workspaces[0].CustomColor != "#196F3D" {
    t.Fatalf("workspaces = %#v", workspaces)
}
```

Add a focused mutation test:

```go
func TestClientSetsExactWorkspaceColor(t *testing.T) {
    server := newRPCServer(t, []rpcExpectation{{
        method: "workspace.action",
        params: map[string]any{
            "action": "set_color", "workspace_id": "w1", "color": "#A04000",
        },
        result: map[string]any{"workspace_id": "w1", "color": "#A04000"},
    }})

    if err := NewClient(server.path).SetWorkspaceColor(
        context.Background(), "w1", "#A04000",
    ); err != nil {
        t.Fatal(err)
    }
}
```

- [ ] **Step 2: Run the client tests and verify RED**

Run:

```sh
go test ./internal/cmux -run 'TestClient(ListsCapabilitiesWorkspacesAndSurfaces|SetsExactWorkspaceColor)$' -count=1
```

Expected: build failure because `Workspace.CustomColor` and
`Client.SetWorkspaceColor` do not exist.

- [ ] **Step 3: Implement the minimal client surface**

Extend `Workspace` without exposing color outside the cmux boundary:

```go
type Workspace struct {
    ID             string `json:"id"`
    Title          string `json:"title"`
    CustomTitle    string `json:"custom_title"`
    HasCustomTitle bool   `json:"has_custom_title"`
    CustomColor    string `json:"custom_color"`
}
```

Add the exact mutation beside the rename methods:

```go
// SetWorkspaceColor assigns one exact hex color to a cmux workspace.
func (c *Client) SetWorkspaceColor(ctx context.Context, workspaceID, color string) error {
    return c.call(ctx, "workspace.action", map[string]any{
        "action":       "set_color",
        "workspace_id": workspaceID,
        "color":        color,
    }, &struct{}{})
}
```

- [ ] **Step 4: Run the focused and package tests and verify GREEN**

Run:

```sh
gofmt -w internal/cmux/client.go internal/cmux/client_test.go
go test ./internal/cmux -run 'TestClient(ListsCapabilitiesWorkspacesAndSurfaces|SetsExactWorkspaceColor)$' -count=1
go test ./internal/cmux -count=1
```

Expected: all commands pass.

- [ ] **Step 5: Commit the protocol increment**

```sh
git add internal/cmux/client.go internal/cmux/client_test.go
git commit -m "feat: support cmux workspace colors"
```

---

### Task 2: Reconcile Agent Workspace Colors from Transcript Activity

**Files:**
- Modify: `internal/cmux/reconciler.go:16-236`
- Test: `internal/cmux/reconciler_test.go:16-277`

**Interfaces:**
- Consumes: `Workspace.CustomColor`, `API.SetWorkspaceColor(ctx, workspaceID, color) error`, `store.Store.GetSession`, exact `liveSnapshot.states`, and injected `Reconciler.now`.
- Produces: `workspaceActivityColor(now, lastActive time.Time) string` and `Reconciler.syncWorkspaceColors(ctx, snapshot) (bool, error)`.

- [ ] **Step 1: Write a failing pure threshold table test**

Add to `reconciler_test.go`:

```go
func TestWorkspaceActivityColorBoundaries(t *testing.T) {
    now := time.Date(2026, 7, 16, 16, 0, 0, 0, time.UTC)
    tests := []struct {
        name string
        at   time.Time
        want string
    }{
        {name: "future", at: now.Add(time.Minute), want: workspaceGreen},
        {name: "current", at: now, want: workspaceGreen},
        {name: "one hour", at: now.Add(-time.Hour), want: workspaceGreen},
        {name: "after one hour", at: now.Add(-time.Hour - time.Nanosecond), want: workspaceOrange},
        {name: "before five hours", at: now.Add(-5*time.Hour + time.Nanosecond), want: workspaceOrange},
        {name: "five hours", at: now.Add(-5 * time.Hour), want: workspaceRed},
        {name: "after five hours", at: now.Add(-8 * time.Hour), want: workspaceRed},
    }
    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            if got := workspaceActivityColor(now, test.at); got != test.want {
                t.Fatalf("workspaceActivityColor() = %q, want %q", got, test.want)
            }
        })
    }
}
```

- [ ] **Step 2: Run the threshold test and verify RED**

Run:

```sh
go test ./internal/cmux -run TestWorkspaceActivityColorBoundaries -count=1
```

Expected: build failure because the constants and function do not exist.

- [ ] **Step 3: Implement the pure fixed policy**

Add these unexported constants and function to `reconciler.go`:

```go
const (
    workspaceGreen  = "#196F3D"
    workspaceOrange = "#A04000"
    workspaceRed    = "#C0392B"
)

func workspaceActivityColor(now, lastActive time.Time) string {
    age := now.Sub(lastActive)
    switch {
    case age <= time.Hour:
        return workspaceGreen
    case age < 5*time.Hour:
        return workspaceOrange
    default:
        return workspaceRed
    }
}
```

- [ ] **Step 4: Verify the pure policy is GREEN**

Run:

```sh
gofmt -w internal/cmux/reconciler.go internal/cmux/reconciler_test.go
go test ./internal/cmux -run TestWorkspaceActivityColorBoundaries -count=1
```

Expected: pass.

- [ ] **Step 5: Write failing reconciliation behavior tests**

Extend the fake API with color calls and per-workspace failures:

```go
type colorCall struct {
    workspaceID string
    color       string
}

// Add to fakeAPI:
colorCalls  []colorCall
colorErrors map[string]error

func (f *fakeAPI) SetWorkspaceColor(_ context.Context, workspaceID, color string) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    f.colorCalls = append(f.colorCalls, colorCall{workspaceID: workspaceID, color: color})
    if err := f.colorErrors[workspaceID]; err != nil {
        return err
    }
    for index := range f.workspaces {
        if f.workspaces[index].ID == workspaceID {
            f.workspaces[index].CustomColor = color
        }
    }
    return nil
}
```

Add an activity-aware fixture helper while retaining existing call sites:

```go
func upsertAnalyzedSessionAt(
    t *testing.T, database *store.Store, id, agent, nativeID, title, status string,
    lastActive time.Time,
) {
    t.Helper()
    started := lastActive.Add(-time.Hour)
    session := store.Session{
        ID: id, Agent: agent, NativeSessionID: nativeID, SourcePath: "/tmp/" + id,
        SourceSize: 1, SourceMTime: lastActive, SourceHash: "hash-" + id,
        WorkingDirectory: "/tmp/project", StartedAt: started, LastActiveAt: lastActive,
    }
    if err := database.UpsertSession(context.Background(), session); err != nil {
        t.Fatal(err)
    }
    if err := database.ReplaceAnalysis(context.Background(), id, store.Analysis{
        Title: title, Summary: "summary", Status: "current", Provider: "fake", Model: "test",
        PromptVersion: "v1", AnalyzedAt: lastActive, AnalyzedHash: "analysis-" + id,
    }); err != nil {
        t.Fatal(err)
    }
    if status != "current" {
        if err := database.SetAnalysisStatus(context.Background(), id, status, ""); err != nil {
            t.Fatal(err)
        }
    }
}
```

Make the existing helper delegate to `upsertAnalyzedSessionAt` with its current
fixed timestamp so existing title tests retain the same semantics.

Add these behavior tests:

```go
func TestActivityColorsUseNewestMappedSessionAndSkipUnmatchedWorkspaces(t *testing.T) {
    now := time.Date(2026, 7, 16, 16, 0, 0, 0, time.UTC)
    database := openReconcilerStore(t)
    upsertAnalyzedSessionAt(t, database, "old", "codex", "n1", "Old", "current", now.Add(-8*time.Hour))
    upsertAnalyzedSessionAt(t, database, "new", "claude", "n2", "New", "current", now.Add(-2*time.Hour))
    home := t.TempDir()
    writeHookFixture(t, home, "codex", hookJSON("n1", "w1", "s1", "idle"))
    writeHookFixture(t, home, "claude", hookJSON("n2", "w1", "s2", "needsInput"))
    api := &fakeAPI{
        capabilities: Capabilities{AccessMode: "allowAll"},
        workspaces: []Workspace{{ID: "w1"}, {ID: "terminal-only", CustomColor: "#1565C0"}},
        surfaces: map[string][]Surface{
            "w1": {{ID: "s1"}, {ID: "s2"}}, "terminal-only": {{ID: "shell"}},
        },
    }
    reconciler := NewReconciler(api, database, home, slog.Default())
    reconciler.now = func() time.Time { return now }

    if err := reconciler.Refresh(context.Background()); err != nil {
        t.Fatal(err)
    }
    if len(api.colorCalls) != 1 || api.colorCalls[0] != (colorCall{workspaceID: "w1", color: workspaceOrange}) {
        t.Fatalf("color calls = %#v", api.colorCalls)
    }
    if api.workspaces[1].CustomColor != "#1565C0" {
        t.Fatalf("terminal-only color changed to %q", api.workspaces[1].CustomColor)
    }
}
```

```go
func TestActivityColorsSkipCorrectColorAndReplaceDifferentColor(t *testing.T) {
    now := time.Date(2026, 7, 16, 16, 0, 0, 0, time.UTC)
    tests := []struct {
        name         string
        currentColor string
        wantCalls    int
    }{
        {name: "already correct", currentColor: workspaceGreen, wantCalls: 0},
        {name: "replace manual color", currentColor: "#1565C0", wantCalls: 1},
    }
    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            database := openReconcilerStore(t)
            upsertAnalyzedSessionAt(t, database, "session", "codex", "native", "Title", "current", now.Add(-30*time.Minute))
            home := t.TempDir()
            writeHookFixture(t, home, "codex", hookJSON("native", "w1", "s1", "idle"))
            api := &fakeAPI{
                capabilities: Capabilities{AccessMode: "allowAll"},
                workspaces:   []Workspace{{ID: "w1", CustomColor: test.currentColor}},
                surfaces:     map[string][]Surface{"w1": {{ID: "s1"}}},
            }
            reconciler := NewReconciler(api, database, home, slog.Default())
            reconciler.now = func() time.Time { return now }

            if err := reconciler.Refresh(context.Background()); err != nil {
                t.Fatal(err)
            }
            if len(api.colorCalls) != test.wantCalls {
                t.Fatalf("color calls = %#v, want %d", api.colorCalls, test.wantCalls)
            }
            if test.wantCalls == 1 && api.colorCalls[0].color != workspaceGreen {
                t.Fatalf("replacement color = %q", api.colorCalls[0].color)
            }
        })
    }
}

func TestActivityColorFailureDoesNotStopOtherWorkspaces(t *testing.T) {
    now := time.Date(2026, 7, 16, 16, 0, 0, 0, time.UTC)
    database := openReconcilerStore(t)
    upsertAnalyzedSessionAt(t, database, "one", "codex", "n1", "One", "current", now.Add(-30*time.Minute))
    upsertAnalyzedSessionAt(t, database, "two", "claude", "n2", "Two", "current", now.Add(-6*time.Hour))
    home := t.TempDir()
    writeHookFixture(t, home, "codex", hookJSON("n1", "w1", "s1", "running"))
    writeHookFixture(t, home, "claude", hookJSON("n2", "w2", "s2", "idle"))
    api := &fakeAPI{
        capabilities: Capabilities{AccessMode: "allowAll"},
        workspaces:   []Workspace{{ID: "w1"}, {ID: "w2"}},
        surfaces: map[string][]Surface{
            "w1": {{ID: "s1"}}, "w2": {{ID: "s2"}},
        },
        colorErrors: map[string]error{"w1": errors.New("rejected")},
    }
    reconciler := NewReconciler(api, database, home, slog.Default())
    reconciler.now = func() time.Time { return now }

    err := reconciler.Refresh(context.Background())
    if err == nil || !strings.Contains(err.Error(), "w1") {
        t.Fatalf("Refresh error = %v", err)
    }
    if len(api.colorCalls) != 2 || api.colorCalls[0].workspaceID != "w1" || api.colorCalls[1].workspaceID != "w2" {
        t.Fatalf("color calls = %#v", api.colorCalls)
    }
    if api.workspaces[1].CustomColor != workspaceRed {
        t.Fatalf("successful workspace color = %q", api.workspaces[1].CustomColor)
    }
}

func TestRefreshWithoutSyncDoesNotWriteActivityColors(t *testing.T) {
    now := time.Date(2026, 7, 16, 16, 0, 0, 0, time.UTC)
    database := openReconcilerStore(t)
    upsertAnalyzedSessionAt(t, database, "session", "codex", "native", "Title", "current", now)
    home := t.TempDir()
    writeHookFixture(t, home, "codex", hookJSON("native", "w1", "s1", "running"))
    api := &fakeAPI{
        capabilities: Capabilities{AccessMode: "allowAll"},
        workspaces:   []Workspace{{ID: "w1"}},
        surfaces:     map[string][]Surface{"w1": {{ID: "s1"}}},
    }
    reconciler := NewReconciler(api, database, home, slog.Default())
    reconciler.now = func() time.Time { return now }

    if err := reconciler.RefreshWithoutSync(context.Background()); err != nil {
        t.Fatal(err)
    }
    if len(api.colorCalls) != 0 {
        t.Fatalf("color calls = %#v, want none", api.colorCalls)
    }
}
```

- [ ] **Step 6: Run reconciliation tests and verify RED**

Run:

```sh
go test ./internal/cmux -run 'Test(ActivityColors|ActivityColorFailure|RefreshWithoutSync)' -count=1
```

Expected: build failure because `API.SetWorkspaceColor` is missing and no
reconciler color writes occur.

- [ ] **Step 7: Implement workspace aggregation and color writes**

Extend the `API` and live snapshot:

```go
type API interface {
    Capabilities(context.Context) (Capabilities, error)
    Workspaces(context.Context) ([]Workspace, error)
    Surfaces(context.Context, string) ([]Surface, error)
    RenameWorkspace(context.Context, string, string) error
    RenameSurface(context.Context, string, string, string) error
    SetWorkspaceColor(context.Context, string, string) error
}

type liveSnapshot struct {
    states              map[string]store.CmuxSessionState
    sessionsInWorkspace map[string]int
    workspaces          map[string]Workspace
}
```

Assign `workspaces: workspaceByID` when constructing the snapshot. Add a
deterministically ordered writer:

```go
func (r *Reconciler) syncWorkspaceColors(ctx context.Context, snapshot liveSnapshot) (bool, error) {
    latest := make(map[string]time.Time)
    var syncErrors []error
    for sessionID, observed := range snapshot.states {
        detail, err := r.store.GetSession(ctx, sessionID)
        if err != nil {
            syncErrors = append(syncErrors, fmt.Errorf("load activity for session %s: %w", sessionID, err))
            continue
        }
        if detail.Session.LastActiveAt.After(latest[observed.WorkspaceID]) {
            latest[observed.WorkspaceID] = detail.Session.LastActiveAt
        }
    }

    workspaceIDs := make([]string, 0, len(latest))
    for workspaceID := range latest {
        workspaceIDs = append(workspaceIDs, workspaceID)
    }
    sort.Strings(workspaceIDs)

    wrote := false
    now := r.now().UTC()
    for _, workspaceID := range workspaceIDs {
        desired := workspaceActivityColor(now, latest[workspaceID])
        if strings.EqualFold(strings.TrimSpace(snapshot.workspaces[workspaceID].CustomColor), desired) {
            continue
        }
        if err := r.api.SetWorkspaceColor(ctx, workspaceID, desired); err != nil {
            syncErrors = append(syncErrors, fmt.Errorf("set cmux workspace color %s: %w", workspaceID, err))
            continue
        }
        wrote = true
    }
    return wrote, errors.Join(syncErrors...)
}
```

Import `sort`. Restructure `refreshLocked` so colors always run on a normal
refresh, title sync remains conditional, and any successful write triggers one
final read-only snapshot:

```go
func (r *Reconciler) refreshLocked(ctx context.Context, allowWrites bool) error {
    snapshot, err := r.snapshotLocked(ctx)
    if err != nil {
        return err
    }
    if !allowWrites {
        return nil
    }

    wrote, syncErr := r.syncWorkspaceColors(ctx, snapshot)
    enabled, err := r.autoSyncEnabled(ctx)
    if err != nil {
        syncErr = errors.Join(syncErr, err)
    } else if enabled {
        titleWrote, err := r.syncEligibleTitles(ctx, snapshot)
        wrote = wrote || titleWrote
        syncErr = errors.Join(syncErr, err)
    }
    if wrote {
        if _, err := r.snapshotLocked(ctx); err != nil {
            syncErr = errors.Join(syncErr, err)
        }
    }
    return syncErr
}
```

Update comments from title-only ownership to cmux state/title/color ownership.

- [ ] **Step 8: Verify reconciler GREEN and guard title regressions**

Run:

```sh
gofmt -w internal/cmux/reconciler.go internal/cmux/reconciler_test.go
go test ./internal/cmux -run 'Test(WorkspaceActivityColor|ActivityColors|ActivityColorFailure|RefreshWithoutSync)' -count=1
go test ./internal/cmux -count=1
```

Expected: all tests pass, including existing automatic and manual title tests.

- [ ] **Step 9: Commit the reconciler increment**

```sh
git add internal/cmux/reconciler.go internal/cmux/reconciler_test.go
git commit -m "feat: color cmux workspaces by agent activity"
```

---

### Task 3: Add Browser-Local Auto-Refresh Control

**Files:**
- Modify: `internal/httpapi/assets/index.html:7-20`
- Modify: `internal/httpapi/assets/app.js:3-8,132-190,220-244,498-564`
- Modify: `internal/httpapi/assets/app.css:38-43,148-153`
- Test: `internal/httpapi/api_test.go:70-171`

**Interfaces:**
- Consumes: current `refreshTimer`, `state.pollTimer`, `startRefreshCountdown`, `refreshPageData`, and `selectSession` behavior.
- Produces: `state.autoRefresh`, `state.selectedAnalysisStatus`, `loadAutoRefreshPreference() bool`, `saveAutoRefreshPreference(bool)`, `stopScheduledRefreshes()`, and `scheduleSelectedSessionPoll()`.

- [ ] **Step 1: Write failing embedded-UI contract assertions**

Change asset-version assertions from `v=2` to `v=3`. Add
`auto-refresh-toggle` to the required HTML IDs and these JavaScript/CSS
fragments to `TestWebApplicationIncludesCoreWorkflows`:

Add `"auto-refresh-toggle"` beside the existing refresh IDs in the current ID
table:

```go
"refresh-label", "refresh-countdown", "auto-refresh-toggle",
```

```go
for _, behavior := range []string{
    `const autoRefreshStorageKey = "agent-history.auto-refresh";`,
    `function loadAutoRefreshPreference()`,
    `function saveAutoRefreshPreference(enabled)`,
    `function stopScheduledRefreshes()`,
    `function scheduleSelectedSessionPoll()`,
    `if (!state.autoRefresh)`,
    `state.selectedAnalysisStatus = session.analysis_status;`,
    `elements["auto-refresh-toggle"].addEventListener("change"`,
} {
    if !strings.Contains(script, behavior) {
        t.Errorf("embedded JavaScript missing auto-refresh behavior %q", behavior)
    }
}
```

```go
for _, style := range []string{
    ".auto-refresh-control {",
    "width: 104px;",
    "white-space: nowrap;",
} {
    if !strings.Contains(styles, style) {
        t.Errorf("embedded CSS missing auto-refresh style %q", style)
    }
}
```

- [ ] **Step 2: Run the web contract tests and verify RED**

Run:

```sh
go test ./internal/httpapi -run 'Test(EmbeddedWebApplication|WebApplicationIncludesCoreWorkflows)$' -count=1
```

Expected: failures for `v=3`, the missing checkbox, preference functions, and
CSS class.

- [ ] **Step 3: Add the stable checkbox markup and styles**

In `index.html`, bump both asset URLs to `v=3` and place this label immediately
before the Refresh button:

```html
<label class="auto-refresh-control">
  <input id="auto-refresh-toggle" type="checkbox" checked>
  <span>Auto refresh</span>
</label>
<button id="refresh-button" type="button"><span id="refresh-label">Refresh</span><span id="refresh-countdown" class="refresh-countdown">in 60s</span></button>
```

In `app.css`, add:

```css
.auto-refresh-control { width: 104px; min-height: 34px; display: inline-flex; align-items: center; gap: 6px; color: var(--text-muted); font-size: 12px; font-weight: 650; white-space: nowrap; }
.auto-refresh-control input { min-height: auto; margin: 0; }
```

Retain the existing `#refresh-button` width and narrow header wrapping rules.

- [ ] **Step 4: Implement preference storage and timer ownership**

At the top of `app.js`, before `state`:

```js
const autoRefreshStorageKey = "agent-history.auto-refresh";

function loadAutoRefreshPreference() {
  try { return localStorage.getItem(autoRefreshStorageKey) !== "off"; }
  catch { return true; }
}

function saveAutoRefreshPreference(enabled) {
  try { localStorage.setItem(autoRefreshStorageKey, enabled ? "on" : "off"); }
  catch { /* Browser storage is optional. */ }
}
```

Extend state:

```js
const state = {
  sessions: [], nextCursor: "", selectedID: "", searchAbort: null,
  pollTimer: null, cmuxStatus: null,
  autoRefresh: loadAutoRefreshPreference(), selectedAnalysisStatus: ""
};
```

Replace timer handling with:

```js
function stopScheduledRefreshes() {
  clearInterval(refreshTimer);
  clearTimeout(state.pollTimer);
  state.pollTimer = null;
  elements["refresh-label"].textContent = "Refresh";
  elements["refresh-countdown"].textContent = "";
}

function scheduleSelectedSessionPoll() {
  clearTimeout(state.pollTimer);
  state.pollTimer = null;
  if (!state.autoRefresh || !state.selectedID ||
      (state.selectedAnalysisStatus !== "queued" && state.selectedAnalysisStatus !== "running")) return;
  const id = state.selectedID;
  state.pollTimer = setTimeout(() => {
    if (state.selectedID === id) {
      void selectSession(id, false);
      void loadSessions(false);
    }
  }, 1800);
}

function startRefreshCountdown() {
  clearInterval(refreshTimer);
  if (!state.autoRefresh) {
    stopScheduledRefreshes();
    return;
  }
  refreshSeconds = refreshIntervalSeconds;
  updateRefreshButton();
  refreshTimer = setInterval(() => {
    refreshSeconds -= 1;
    if (refreshSeconds <= 0) {
      void refreshPageData(false);
      return;
    }
    updateRefreshButton();
  }, 1000);
}
```

In `selectSession`, replace the inline queued/running timeout with:

```js
state.selectedAnalysisStatus = session.analysis_status;
scheduleSelectedSessionPoll();
```

The existing `refreshPageData` `finally` block may continue calling
`startRefreshCountdown`; the function now preserves the disabled state.

Register and initialize the control:

```js
elements["auto-refresh-toggle"].checked = state.autoRefresh;
elements["auto-refresh-toggle"].addEventListener("change", (event) => {
  state.autoRefresh = event.currentTarget.checked;
  saveAutoRefreshPreference(state.autoRefresh);
  if (state.autoRefresh) {
    startRefreshCountdown();
    scheduleSelectedSessionPoll();
  } else {
    stopScheduledRefreshes();
  }
});
```

- [ ] **Step 5: Verify focused UI contracts and JavaScript syntax GREEN**

Run:

```sh
node --check internal/httpapi/assets/app.js
go test ./internal/httpapi -run 'Test(EmbeddedWebApplication|WebApplicationIncludesCoreWorkflows)$' -count=1
```

Expected: pass.

- [ ] **Step 6: Commit the browser increment**

```sh
git add internal/httpapi/assets/index.html internal/httpapi/assets/app.js internal/httpapi/assets/app.css internal/httpapi/api_test.go
git commit -m "feat: add browser auto-refresh control"
```

---

### Task 4: Document, Verify, Deploy, and Publish

**Files:**
- Modify: `README.md:140-199`
- Modify: `run-local.sh:7`
- Verify: all changed code and the installed LaunchAgent service.

**Interfaces:**
- Consumes: completed cmux color reconciler and browser preference.
- Produces: documented `v0.1.8` deployment at `http://127.0.0.1:54321/` and a clean pushed branch.

- [ ] **Step 1: Document the fixed behavior and operational boundary**

Add to the cmux section of `README.md`:

```markdown
Open exactly matched Claude and Codex workspaces are colored from imported
transcript activity: Green through one hour, Orange after one hour and before
five hours, and Red at five hours or later. A shared workspace uses its most
recently active mapped agent. Terminal-only workspaces are not modified.

The header's **Auto refresh** checkbox controls only scheduled browser fetches
and is remembered by that browser. Turning it off also stops selected-analysis
polling; manual Refresh, background history scanning, analysis, and cmux color
reconciliation continue.
```

State that transcript scans run every 15 minutes, so activity-color changes can
lag by one scan interval.

- [ ] **Step 2: Bump the deployed binary version**

Change `run-local.sh`:

```sh
go build -trimpath -ldflags '-X main.version=v0.1.8' -o agent-history ./cmd/agent-history
```

- [ ] **Step 3: Run formatting, syntax, diff, and complete automated verification**

Run:

```sh
gofmt -w internal/cmux/client.go internal/cmux/client_test.go internal/cmux/reconciler.go internal/cmux/reconciler_test.go internal/httpapi/api_test.go
node --check internal/httpapi/assets/app.js
git diff --check
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
go build ./cmd/agent-history
```

Expected: every command passes and `govulncheck` reports no reachable
vulnerabilities. If `govulncheck` is not installed, run the official equivalent:

```sh
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

- [ ] **Step 4: Review the complete diff for scope and privacy**

Run:

```sh
git status --short
git diff --check origin/main...HEAD
git diff --stat origin/main...HEAD
git diff origin/main...HEAD -- internal/cmux internal/httpapi/assets internal/httpapi/api_test.go README.md run-local.sh
```

Verify every changed line traces to the approved spec, no transcript text or
live workspace title is committed, and no unrelated file is modified.

- [ ] **Step 5: Commit documentation and deployment metadata**

```sh
git add README.md run-local.sh
git commit -m "docs: describe agent activity colors"
```

- [ ] **Step 6: Capture live pre-deployment workspace state without titles**

Run:

```sh
cmux rpc system.capabilities '{}' | jq '.methods as $methods | {access_mode, workspace_action:($methods|index("workspace.action") != null)}'
cmux rpc workspace.list '{}' | jq '[.workspaces[] | {id, custom_color}]' > /tmp/agent-history-workspaces-before.json
```

Expected: `access_mode` is `allowAll`, `workspace_action` is `true`, and the
temporary baseline contains IDs/colors only.

- [ ] **Step 7: Restart the installed Agent History LaunchAgent from the committed tree**

Run:

```sh
launchctl kickstart -k "gui/$(id -u)/com.tarunjain.agent-history"
attempts=0
until curl -fsS http://127.0.0.1:54321/api/health >/dev/null; do
  attempts=$((attempts + 1))
  test "$attempts" -lt 100
  sleep 0.1
done
./agent-history version
```

Expected: health returns `{"status":"ok"}` and version is
`agent-history v0.1.8`.

- [ ] **Step 8: Verify live color eligibility without exposing session titles**

Wait for the immediate reconciler pass, then run:

```sh
sleep 2
cmux rpc workspace.list '{}' | jq '[.workspaces[] | {id, custom_color}]' > /tmp/agent-history-workspaces-after.json
jq -n --slurpfile before /tmp/agent-history-workspaces-before.json --slurpfile after /tmp/agent-history-workspaces-after.json '{before_count:($before[0]|length),after_count:($after[0]|length),colored_after:([$after[0][]|select(.custom_color=="#196F3D" or .custom_color=="#A04000" or .custom_color=="#C0392B")]|length)}'
```

Build the expected workspace/color set directly from the reconciler's exact
open-state table and transcript timestamps, then assert both matched and
unmatched behavior without printing titles or transcript content:

```sh
db="$HOME/.local/share/agent-history/history.db"
sqlite3 -json "$db" "
SELECT c.workspace_id AS id,
       CASE
         WHEN (julianday('now') - julianday(max(s.last_active_at))) <= (1.0 / 24.0) THEN '#196F3D'
         WHEN (julianday('now') - julianday(max(s.last_active_at))) < (5.0 / 24.0) THEN '#A04000'
         ELSE '#C0392B'
       END AS custom_color
FROM cmux_session_state c
JOIN sessions s ON s.id = c.session_id
WHERE c.open = 1
GROUP BY c.workspace_id
ORDER BY c.workspace_id;
" > /tmp/agent-history-workspaces-expected.json

jq -e -n \
  --slurpfile before /tmp/agent-history-workspaces-before.json \
  --slurpfile after /tmp/agent-history-workspaces-after.json \
  --slurpfile expected /tmp/agent-history-workspaces-expected.json '
    ($before[0] | map({key:.id, value:.custom_color}) | from_entries) as $beforeByID |
    ($after[0] | map({key:.id, value:.custom_color}) | from_entries) as $afterByID |
    ($expected[0] | map({key:.id, value:.custom_color}) | from_entries) as $expectedByID |
    all(($afterByID | keys[]) as $id;
      if ($expectedByID | has($id))
      then $afterByID[$id] == $expectedByID[$id]
      else $afterByID[$id] == $beforeByID[$id]
      end)
  '
```

Expected: `jq -e` exits zero. Every exact open agent workspace has its computed
color, and every terminal-only or otherwise unmatched workspace retains its
baseline color.

- [ ] **Step 9: Perform desktop and narrow browser acceptance checks**

Open a fresh tab with browser-harness and capture the initial desktop state:

```sh
browser-harness <<'PY'
new_tab("http://127.0.0.1:54321/")
wait_for_load()
capture_screenshot("/tmp/agent-history-auto-refresh-desktop.png")
print(js("""({
  checked: document.querySelector('#auto-refresh-toggle').checked,
  refreshWidth: document.querySelector('#refresh-button').getBoundingClientRect().width,
  overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
  selected: new URL(location.href).searchParams.get('session')
})"""))
PY
```

Expected: checked `true` on first use, refresh width remains `126`, and no
horizontal overflow.

Use a screenshot-driven coordinate click on the checkbox, wait more than one
countdown tick, and verify the countdown is blank, the button says `Refresh`,
the current URL/filter/selection are unchanged, and the stored preference is
`off`. Click manual Refresh and confirm the preference and URL state remain
unchanged. Reload the page and confirm the checkbox remains off.

Set a 390 by 844 viewport through CDP, re-screenshot, and verify the checkbox,
Refresh button, and other header commands wrap without overlap or horizontal
overflow. Turn Auto refresh back on before leaving the page and verify a fresh
60-second countdown appears.

- [ ] **Step 10: Confirm the service and analysis queue remain healthy**

Run:

```sh
curl -fsS http://127.0.0.1:54321/api/health
curl -fsS http://127.0.0.1:54321/api/settings | jq '{cmux_available,cmux_access_mode,cmux_error,cmux_title_sync}'
launchctl print "gui/$(id -u)/com.tarunjain.agent-history" | rg 'state =|pid =|runs ='
sqlite3 "$HOME/.local/share/agent-history/history.db" 'SELECT analysis_status, count(*) FROM sessions GROUP BY analysis_status ORDER BY analysis_status;'
```

Expected: service state is running, cmux is available in `allowAll`, and the
queue contains no unexpected failed state caused by deployment.

- [ ] **Step 11: Push and verify the remote branch**

Run:

```sh
git status --porcelain
git push origin main
local_head=$(git rev-parse HEAD)
remote_head=$(git ls-remote origin refs/heads/main | awk '{print $1}')
test "$local_head" = "$remote_head"
```

Expected: the worktree is clean, push succeeds, and local/remote `main` hashes
match.

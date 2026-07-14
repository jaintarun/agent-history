# Web Auto-Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refresh the current session results and selected detail every 60 seconds, with an immediate header action that preserves filters and selection.

**Architecture:** Add one embedded header control and a small browser timer around the existing `loadSessions(false)` data path. No server API, persistence, dependency, or separate frontend runtime is added.

**Tech Stack:** Go embedded assets, plain HTML, plain browser JavaScript, `httptest` asset-contract tests.

## Global Constraints

- The interval is exactly 60 seconds.
- Manual and automatic refreshes preserve the current filters and selected session.
- The existing queued/running-session poll remains unchanged.
- No HTTP endpoint, setting, dependency, or server-side scheduler is added.

---

### Task 1: Add the refresh control and timer

**Files:**
- Modify: `internal/httpapi/api_test.go`
- Modify: `internal/httpapi/assets/index.html`
- Modify: `internal/httpapi/assets/app.js`
- Modify: `docs/DESIGN.md`

**Interfaces:**
- Consumes: `loadSessions(false)`, which reads the current filter controls and reloads `state.selectedID` detail.
- Produces: `refreshPageData()` and the `refresh-button` header control.

- [x] **Step 1: Write the failing embedded-asset contract test**

Add `refresh-button` to the required HTML control IDs and assert the shipped script contains the fixed interval, timer, refresh function, existing loader call, and click binding:

```go
for _, id := range []string{
	"session-search", "agent-filter", "active-filter", "cwd-filter", "topic-filter",
	"status-filter", "sort-filter", "active-after-filter", "active-before-filter",
	"started-after-filter", "started-before-filter", "session-results", "detail-content",
	"confirm-dialog", "command-dialog", "settings-dialog", "retitle-weak-button", "refresh-button",
} {
	if !strings.Contains(markup, `id="`+id+`"`) {
		t.Errorf("embedded HTML missing control %q", id)
	}
}
for _, behavior := range []string{
	"const refreshIntervalSeconds = 60;",
	"function startRefreshCountdown()",
	"async function refreshPageData()",
	"await loadSessions(false);",
	`elements["refresh-button"].addEventListener("click", refreshPageData);`,
} {
	if !strings.Contains(script, behavior) {
		t.Errorf("embedded JavaScript missing refresh behavior %q", behavior)
	}
}
```

- [x] **Step 2: Run the focused test and confirm it fails for the missing control**

Run: `go test ./internal/httpapi -run TestWebApplicationIncludesCoreWorkflows -count=1`

Expected: FAIL reporting that `refresh-button` and the refresh JavaScript behaviors are missing.

- [x] **Step 3: Add the header control**

Add the control before the existing header actions:

```html
<button id="refresh-button" type="button">Refresh in 60s</button>
```

- [x] **Step 4: Add the minimal countdown and refresh behavior**

Add a fixed interval, update function, countdown starter, and refresh action. The action delegates to the existing loader so filter values, URL state, and `state.selectedID` remain unchanged:

```javascript
const refreshIntervalSeconds = 60;
let refreshSeconds = refreshIntervalSeconds;
let refreshTimer;

function updateRefreshButton() {
  elements["refresh-button"].textContent = `Refresh in ${refreshSeconds}s`;
}

function startRefreshCountdown() {
  clearInterval(refreshTimer);
  refreshSeconds = refreshIntervalSeconds;
  updateRefreshButton();
  refreshTimer = setInterval(() => {
    refreshSeconds -= 1;
    if (refreshSeconds <= 0) {
      void refreshPageData();
      return;
    }
    updateRefreshButton();
  }, 1000);
}

async function refreshPageData() {
  clearInterval(refreshTimer);
  elements["refresh-button"].disabled = true;
  elements["refresh-button"].textContent = "Refreshing...";
  try {
    await loadSessions(false);
  } finally {
    elements["refresh-button"].disabled = false;
    startRefreshCountdown();
  }
}
```

Bind the click and start the timer after initial loading:

```javascript
elements["refresh-button"].addEventListener("click", refreshPageData);
restoreFilters();
renderFilterChips();
loadFacets();
loadSessions(false);
startRefreshCountdown();
```

- [x] **Step 5: Document the shipped embedded-page behavior**

Add this capability to the embedded web application list in `docs/DESIGN.md`:

```markdown
- one-minute in-place data refresh with a manual countdown control that
  preserves current filters and selection;
```

- [x] **Step 6: Run focused and repository verification**

Run:

```sh
gofmt -w internal/httpapi/api_test.go
go test ./internal/httpapi -run TestWebApplicationIncludesCoreWorkflows -count=1
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/agent-history
node --check internal/httpapi/assets/app.js
git diff --check
```

Expected: every available command exits successfully with no test failures or JavaScript syntax errors.

- [ ] **Step 7: Rebuild, restart, and verify in a browser**

Run `./run-local.sh`, open `http://127.0.0.1:54321/`, and confirm:

- the countdown decrements;
- clicking refresh loads immediately and resets it to 60 seconds;
- search/filter values and selected session remain unchanged;
- the automatic refresh occurs at zero;
- header controls fit at desktop and narrow viewports without overlap.

Automated visual verification was blocked because Chrome required local remote
debugging approval and no in-app browser instance was available. The rebuilt
live assets and service health were verified over HTTP, and the page was opened
in the system browser for manual viewing.

- [x] **Step 8: Commit**

```sh
git add internal/httpapi/api_test.go internal/httpapi/assets/index.html internal/httpapi/assets/app.js docs/DESIGN.md docs/superpowers/plans/2026-07-14-web-auto-refresh.md
git commit -m "Add automatic web result refresh"
```

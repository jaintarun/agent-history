# cmux Agent Activity Colors and Web Auto-Refresh Control

## Goal

Make open AI-agent workspaces in cmux communicate transcript recency at a
glance, and let each browser disable Agent History's scheduled page refreshes
without affecting server-side processing.

Agent History will color only cmux workspaces containing an exact open Claude
or Codex session mapping. It will use the most recently active mapped session
when a workspace contains more than one agent session. Ordinary terminal-only
workspaces remain untouched.

## Scope

This feature will:

- derive activity age from the imported session transcript's
  `sessions.last_active_at` value;
- assign cmux Green, Orange, or Red to exact open agent workspaces;
- refresh those colors through the existing one-minute cmux reconciler;
- let Agent History own and replace the color of a matched agent workspace;
- avoid redundant color writes when the current cmux color is already correct;
- add a browser-local **Auto refresh** checkbox next to the manual Refresh
  button; and
- preserve the existing filters, URL state, selected session, and manual
  refresh behavior.

This feature will not:

- color workspaces that contain no exact open agent mapping;
- infer agent identity from a title, directory, process name, or timestamp;
- treat cmux lifecycle or hook-file update timestamps as conversation activity;
- add configurable activity thresholds or colors;
- add a server-side setting for browser auto-refresh;
- change the 15-minute transcript scan interval; or
- make browser auto-refresh control server scanning, analysis, or cmux
  reconciliation.

## Activity Semantics

`sessions.last_active_at` is the authoritative activity timestamp. Importers
derive it from persisted transcript messages, so it represents text or events
written to the agent conversation rather than a cmux lifecycle transition such
as entering an idle state.

For an activity timestamp `lastActive` and reconciliation time `now`, the
desired color is:

| Activity age | cmux color | Built-in hex |
|---|---|---|
| `now - lastActive <= 1 hour` | Green | `#196F3D` |
| `1 hour < now - lastActive < 5 hours` | Orange | `#A04000` |
| `now - lastActive >= 5 hours` | Red | `#C0392B` |

A future timestamp is treated as zero age and therefore Green. Exact boundary
behavior is intentional: one hour is Green and five hours is Red.

The source scanner currently imports changed transcripts every 15 minutes.
Color transitions can consequently lag transcript activity by roughly one scan
interval. The cmux hook `updatedAt` field will not be used as a faster fallback
because non-conversation lifecycle changes can update it and incorrectly make
an inactive agent appear Green.

## Workspace Selection

The existing exact join remains the only eligibility path:

```text
(agent, native session ID) -> (cmux workspace ID, cmux surface ID)
```

A mapping is open only when both its workspace and surface exist in the live
cmux snapshot. Claude and Codex mappings are supported through their existing
hook-state readers.

An open mapped session participates whether its lifecycle is `running`,
`idle`, `needsInput`, or `unknown`. This is necessary because an old idle or
waiting agent is precisely what Orange and Red need to identify.

When multiple mapped agent sessions share a workspace, the reconciler chooses
the greatest `last_active_at`. The workspace color therefore answers, "When
was any mapped agent in this workspace most recently active?"

Workspaces without an exact open agent mapping are not modified. Agent History
does not clear or restore their current color.

## cmux Client Changes

The existing `internal/cmux` client will expand only at its cmux protocol
boundary:

- decode `custom_color` from `workspace.list`; and
- add a workspace color operation using:

```json
{
  "method": "workspace.action",
  "params": {
    "action": "set_color",
    "workspace_id": "<exact workspace id>",
    "color": "#RRGGBB"
  }
}
```

The operation will use fixed cmux built-in hex values rather than palette names
so comparisons are deterministic and user palette overrides cannot cause a
write on every reconciliation. As with title operations, requests use bounded
socket deadlines and never accept a browser-supplied workspace ID or color.

No database migration is required. The current live `custom_color` is enough
to suppress redundant writes, and the approved policy allows Agent History to
replace any differing color while the workspace contains a mapped agent.

## Reconciler Changes

The existing serialized reconciler remains the single cmux writer. A normal
refresh will:

1. read capabilities, workspaces, surfaces, and exact hook mappings;
2. persist the live open/title/lifecycle snapshot as it does today;
3. load `last_active_at` for every exact open mapped Agent History session;
4. group sessions by workspace and retain the newest activity timestamp;
5. compute the desired fixed color at the injected reconciliation clock;
6. compare it with the live workspace `custom_color`; and
7. call `workspace.action set_color` only for differing matched workspaces.

Color reconciliation is always enabled when cmux is available in `allowAll`
mode. It is independent of the optional `cmux.title_sync` setting. A method
whose purpose is explicitly to refresh state without writes will continue to
perform no title or color writes.

Color writes and title writes share the reconciler mutex, so timer refreshes,
manual refreshes, manual title pushes, and automatic writes cannot race.

If one workspace color write fails, the reconciler continues attempting other
eligible workspaces and joins the errors for logging and the synchronous
refresh caller. The successfully captured cmux snapshot remains valid and cmux
does not become globally unavailable solely because one color mutation failed.
Raw socket paths remain absent from browser API responses.

## Web Auto-Refresh Control

The embedded page will add a checked checkbox labeled **Auto refresh** beside
the existing fixed-width Refresh button.

The preference is browser-local:

- the first visit defaults to enabled;
- changes are stored under a namespaced `localStorage` key;
- malformed, unavailable, or inaccessible storage falls back to enabled; and
- no HTTP setting or SQLite row is added.

When enabled:

- the existing 60-second countdown is visible;
- reaching zero performs the current page refresh; and
- queued or running selected-session detail polling continues.

When disabled:

- the countdown interval is cleared;
- the selected-session polling timeout is cleared;
- the Refresh button remains enabled and displays only `Refresh`;
- manual Refresh still reloads cmux status, the current result page, and the
  selected detail while preserving filters and selection; and
- completing a manual refresh does not silently re-enable the countdown.

Turning Auto refresh back on starts a new 60-second countdown. It does not
immediately fetch or alter the URL, filters, current result set, or selected
session.

The backend LaunchAgent, transcript scanner, analysis queue, and cmux
reconciler continue running regardless of this browser setting.

The checkbox and label will have stable dimensions, remain adjacent to the
Refresh control when space permits, and wrap with the existing header actions
without causing horizontal overflow at narrow viewport widths.

## API Surface

No new Agent History HTTP endpoints or JSON settings are needed. The existing
manual `POST /api/cmux/refresh` endpoint invokes the enhanced reconciler and
therefore also updates eligible colors.

The cmux workspace structure gains `custom_color` internally. Agent History's
session JSON does not need to expose it because the requested color is visible
in cmux and has no browser workflow.

## Testing

Deterministic tests will cover:

- decoding `custom_color` from `workspace.list`;
- the exact `workspace.action` method and `set_color` parameters;
- Green at zero, future, and exactly one-hour age;
- Orange immediately after one hour and immediately before five hours;
- Red at and after five hours;
- selection of the newest activity across multiple sessions in one workspace;
- idle and waiting mapped agents participating;
- unmatched sessions and terminal-only workspaces receiving no color calls;
- an already-correct color receiving no write;
- a differing manual color being replaced for a matched workspace;
- one failed workspace write not preventing other workspace attempts;
- checkbox markup, default-on behavior, storage restoration, timer shutdown,
  timer restart, manual refresh while disabled, and selected-detail polling
  suppression; and
- stable responsive layout in desktop and narrow browser viewports.

Tests use fake cmux protocol servers and temporary real SQLite databases. They
do not invoke real models or mutate live cmux workspaces.

## Deployment and Acceptance

After automated verification, the LaunchAgent will be restarted from the
committed repository so the new binary runs at `127.0.0.1:54321`. Acceptance
checks will confirm:

- live cmux reports `allowAll` and supports `workspace.action`;
- exact open agent workspaces receive the expected color for imported activity;
- terminal-only workspaces retain their existing colors;
- disabling Auto refresh stops scheduled network requests while manual Refresh
  still preserves the current filter and selected session; and
- the page has no overlap or horizontal scrolling in desktop and narrow
  viewports.

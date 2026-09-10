# cmux Session Status and Metadata Sync

## Goal

Show which Agent History sessions are currently open in cmux, compare the
Agent History title and short summary with the current cmux workspace metadata,
allow an explicit metadata push, and optionally keep eligible cmux titles and
descriptions synchronized automatically.

Agent History remains the source of generated titles and short summaries. cmux
remains the display target, and metadata manually changed in cmux is preserved
unless the user explicitly pushes the Agent History metadata.

## Scope

This feature will:

- connect the continuously running Agent History service to the local cmux API;
- match Claude, Codex, and Grok sessions using native agent session IDs from
  cmux hook state;
- show whether a session is open in cmux and its agent lifecycle;
- show the Agent History and cmux titles and descriptions together when they
  differ;
- add a per-session `Send title and description to cmux` command;
- add an `Automatically sync titles and descriptions to cmux` setting;
- add an `All`, `Open in cmux`, and `Not open in cmux` session filter; and
- retain the existing browser-page refresh, filters, and selected session.

This feature will not:

- embed the Agent History web application inside cmux;
- modify Codex state databases, Claude transcripts, or cmux Vault records;
- implement bidirectional metadata synchronization;
- enable cmux's separate AI workspace auto-naming;
- infer session identity from titles, directories, or timestamps; or
- overwrite independently changed cmux metadata during automatic sync.

## cmux Prerequisite

cmux socket control will be changed from `cmuxOnly` to `allowAll`, as approved.
The installed cmux instance must report `allowAll` before Agent History enables
cmux reads or writes. Agent History will report an actionable unavailable state
when cmux is stopped, its socket is inaccessible, or its access mode is not
compatible.

The access-mode change is a machine setup step, not a setting owned or silently
changed by the Agent History web application. The startup installation will be
verified after the change so the LaunchAgent can reach cmux after login.

## Identity and Live State

cmux's documented hook files under `~/.cmuxterm` provide an exact mapping:

```text
(agent, native session ID) -> (workspace ID, surface ID, lifecycle)
```

Agent History supports the Claude, Codex, and Grok hook files. A mapping counts
as `Open in cmux` only when its workspace and surface still exist in the
current cmux API snapshot. Stale hook records do not count as open.

Lifecycle is separate from open state and is displayed as one of `Running`,
`Idle`, `Needs input`, or `Unknown`. Thus an idle agent remains `Open in cmux`
and appears under that filter.

## Components

### cmux Client

A small `internal/cmux` package will own:

- cmux API availability and access-mode checks;
- workspace and surface listing;
- hook-state parsing and exact session matching;
- workspace renaming;
- workspace description updates; and
- surface/tab renaming.

The client will use cmux's newline-delimited JSON Unix-socket API. It will have
bounded connection and request timeouts and no retries inside a request. Socket
discovery will honor `CMUX_SOCKET_PATH` and then check cmux's stable per-user and
legacy release locations.

No cmux-specific file or protocol handling will leak into the store, HTTP, or UI
packages.

### Reconciler

A single reconciler will refresh cmux state at startup and every 60 seconds. A
refresh will:

1. list current workspaces and their surfaces;
2. read Claude, Codex, and Grok hook mappings;
3. match mappings to Agent History sessions by agent and native session ID;
4. persist the current open/title/description/lifecycle observations; and
5. apply eligible automatic metadata updates when the setting is enabled.

The reconciler will serialize refreshes so the timer, an API-triggered refresh,
and a manual push cannot race. A failed refresh leaves the last observed title
available for diagnosis but marks cmux globally unavailable and does not perform
automatic writes.

### Persistence

A `cmux_session_state` table will store one row per matched Agent History
session:

- session ID;
- workspace ID and surface ID;
- current workspace and surface titles plus the workspace description;
- lifecycle and whether the session is currently open;
- observation time;
- last workspace and surface titles and workspace description Agent History
  successfully pushed; and
- last push time.

Current state makes the `Open in cmux` filter queryable without breaking search
pagination. Last-pushed metadata provides the provenance required to protect a
later manual cmux edit.

## Title and Description Behavior

For the normal one-agent-session workspace, the detail view compares the Agent
History title with the containing cmux workspace title. It also shows the exact
mapped tab title when that differs. For a workspace containing multiple mapped
agent sessions, the exact tab becomes the comparison and synchronization target
because the workspace title is shared. Whitespace-trimmed exact equality is
considered synchronized.

For a single-session workspace, the existing Agent History short summary is the
cmux workspace description. It requires no additional analyzer request. For a
workspace containing multiple mapped sessions, no description is synchronized
because cmux has no per-tab description.

The manual `Send title and description to cmux` command:

- requires a nonempty Agent History title and an open exact mapping;
- renames the mapped tab;
- renames the workspace when that workspace has only one mapped agent session;
- sets that workspace's description from the short summary;
- records each successfully pushed value; and
- is allowed to replace different cmux metadata because it is an explicit user
  action.

For a workspace containing multiple mapped agent sessions, the command renames
only the exact tab. This prevents one session from replacing shared workspace
metadata for the other sessions.

Automatic synchronization runs only when all of these conditions hold:

- the global setting is enabled;
- the Agent History analysis is current and has a nonempty title;
- the session is exactly matched and currently open;
- the workspace contains only one mapped agent session; and
- either cmux has no custom workspace title, or the current cmux title equals
  the last workspace title Agent History successfully pushed.

The same pass sets a nonempty short summary as the workspace description when
the current cmux description is blank or equals the last description Agent
History successfully pushed.

The initial automatic sync renames only the eligible workspace. A tab is
automatically updated only after Agent History has previously pushed that tab
title and the current tab title still equals that last-pushed value. This is
necessary because cmux exposes custom-title ownership for workspaces but not for
tabs. It prevents an automatic pass from overwriting a tab title changed
independently in cmux.

If either current cmux value differs from both the Agent History value and the
last pushed value, the state is `Different - cmux metadata preserved`.
Automatic sync preserves that value and the explicit push button remains
available.

cmux's built-in AI workspace auto-naming remains disabled to avoid competing
writers and duplicate model usage.

## HTTP API

The existing session list and detail responses will gain a nested `cmux` value
when cmux state is known. It contains open state, lifecycle, workspace and
surface titles, workspace description, comparison states, and last observation
time. Internal socket paths are not returned.

The API additions are:

- `POST /api/sessions/{id}/cmux-title` to perform the explicit metadata push;
- `POST /api/cmux/refresh` to request an immediate reconciliation;
- `cmux=open|closed` on `GET /api/sessions`; and
- `cmux_title_sync` on the existing settings GET and PUT endpoints.

The global cmux availability and access mode will be included in settings so the
UI can distinguish `Not open in cmux` from `cmux unavailable`.

## Web UI

The filter bar will add a compact cmux status selector:

- `All sessions`;
- `Open in cmux`; and
- `Not open in cmux`.

The filter participates in the existing URL state, filter chips, automatic
refresh, and selected-session preservation.

Open session rows will show a restrained cmux badge with lifecycle. The selected
session will show a comparison block near its title:

- synchronized: cmux metadata and a `Synced` status;
- different: Agent History and cmux metadata, preservation status, and the
  explicit send button;
- open but no Agent History title: cmux title only;
- not open: `Not open in cmux`; or
- unavailable: the cmux connection error without presenting stale data as live.

The Settings dialog exposes `Automatically sync titles and descriptions to
cmux`. Turning it off stops automatic writes but continues reading, comparison,
filtering, and manual pushes.

## Failure Handling

- cmux stopped or inaccessible: show unavailable; analysis, search, and history
  continue normally.
- access mode not `allowAll`: show the detected mode and the required setting;
  do not write.
- malformed or partial hook file: ignore only that file/snapshot and report a
  concise diagnostic; do not use heuristic matching.
- stale mapping: mark the session not open.
- workspace or tab closes during a push: return a conflict-style error and
  refresh cmux state.
- a workspace title, description, or tab title write succeeds while another
  fails: refresh state, report the partial failure, and do not claim
  synchronization.

No cmux outage may prevent the Agent History server from starting or serving its
existing API and web application.

## Verification

Tests will cover:

- cmux socket framing, timeout, error, listing, and rename requests using a fake
  Unix-socket server;
- Claude, Codex, and Grok hook parsing and exact native-ID matching;
- stale, missing, duplicate, and multi-session workspace mappings;
- database migration, current-state replacement, and open/closed filtering;
- automatic-sync policy for unowned, previously pushed, manually changed,
  partial-analysis, disabled, and multi-session cases;
- manual push success and partial failure;
- API validation, settings persistence, cmux DTOs, and unavailable behavior;
- web controls, URL-preserved filtering, lifecycle badges, comparison states,
  and manual push; and
- the full Go test suite, race detector, static analysis, and live browser
  verification against the local service.

Live acceptance requires one Claude and one Codex session where hooks are
available. A manual cmux rename must remain unchanged during automatic refresh,
then change only after the explicit push command.

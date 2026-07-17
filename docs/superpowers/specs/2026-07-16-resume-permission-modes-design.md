# Resume Permission Modes Design

**Date:** 2026-07-16
**Status:** Implemented

## Purpose

Let a user resume a stored Codex or Claude Code session either with the agent's
normal permission behavior or with that agent's explicit permission-bypass
mode. Both choices must be available from the session detail view without
weakening the launcher's existing trust boundary.

This feature is intended for a public repository. The browser may choose only a
fixed application-owned permission mode; it may never submit command text,
executable paths, or arbitrary CLI flags.

## Scope

Each resumable session exposes two actions based on its source agent:

- Codex: **Resume normally** and **Resume with YOLO**
- Claude Code: **Resume normally** and **Resume with dangerously skipped
  permissions**

The second Codex label uses the familiar "YOLO" wording, but Agent History
generates the stable long-form command:

```text
codex resume --dangerously-bypass-approvals-and-sandbox <session-id>
```

The Claude Code bypass command is:

```text
claude --dangerously-skip-permissions --resume <session-id>
```

Normal resume commands remain unchanged:

```text
codex resume <session-id>
claude --resume <session-id>
```

The selected permission mode applies consistently whether the result launches
in cmux or falls back to a copyable command.

## Non-Goals

This change does not add:

- Chrome sessions or a generic browser/terminal session source;
- a global default permission mode;
- persistence of a user's last permission choice;
- arbitrary command, executable, environment, or flag input;
- cross-agent actions, such as a Claude permission flag on a Codex session;
- a new launcher backend; or
- changes to transcript ingestion, analysis, search, or cmux title and color
  reconciliation.

The bypass action is deliberately explicit for each resume. Normal resume
remains available and remains the backward-compatible default.

## Web Experience

The current single **Resume** action becomes the two agent-specific actions
described above. The UI derives the available labels from the stored session
agent and sends only one of the fixed permission values.

The longer Claude label may wrap within the existing action area, but it must
not overlap adjacent controls or force the page wider than a narrow viewport.
Both buttons remain ordinary explicit commands alongside Analyze, Retitle,
Rescan, and Delete analysis. No confirmation dialog or global toggle is added.

After a click, existing behavior is preserved:

- a successful cmux launch shows the existing launched-session notification;
- a copy fallback opens the existing Resume command dialog with the exact
  generated command; and
- a launch error appears through the existing error notification.

Only Codex and Claude sessions are currently resumable. This design does not
interpret "Chrome" as a session source because Agent History has no Chrome
transcript adapter or trusted Chrome resume specification.

## HTTP Contract

`POST /api/sessions/{id}/launch` accepts:

```json
{
  "launcher": "auto",
  "permissions": "normal"
}
```

The allowed values are:

- `launcher`: the existing `auto`, `cmux`, or `copy`;
- `permissions`: `normal` or `bypass`.

An omitted `permissions` field means `normal` so existing API callers retain
their current behavior. Any other value is rejected as a bad request before a
subprocess is considered. The response shape is unchanged.

The browser sends `launcher: "auto"` and the selected permission enum. It does
not know or transmit the corresponding CLI flag.

## Launcher Design

The trusted launcher boundary gains the permission mode as a separate argument:

```go
Launch(
    context.Context,
    sessionID string,
    launcherMode string,
    permissionMode string,
) (Result, error)
```

The launcher performs these steps in order:

1. Validate the launcher and permission enums.
2. Load the stored session and configured source adapter.
3. Ask the source adapter for its normal `ResumeSpec`.
4. Validate that normal specification against the stored agent, native session
   ID, working directory, executable, and exact expected normal arguments.
5. Copy the validated arguments and, only for `bypass`, add the fixed
   application-owned flag for the validated agent.
6. Safely render the resulting command for the copy response or internally
   generated cmux command.

The source adapters continue to describe only normal resume behavior. They do
not accept a permission mode and do not construct dangerous variants. This
keeps validation centralized: permission bypass is a small, auditable transform
applied only after the source adapter's output is proven trustworthy.

Argument construction is exact:

| Agent | Permission mode | Arguments after the executable |
| --- | --- | --- |
| Codex | `normal` | `resume`, `<session-id>` |
| Codex | `bypass` | `resume`, `--dangerously-bypass-approvals-and-sandbox`, `<session-id>` |
| Claude | `normal` | `--resume`, `<session-id>` |
| Claude | `bypass` | `--dangerously-skip-permissions`, `--resume`, `<session-id>` |

The implementation copies argument slices before modification so a source
adapter's `ResumeSpec` cannot be mutated accidentally.

## Safety And Error Handling

Permission bypass grants the resumed agent the authority represented by the
vendor CLI flag. Agent History makes that choice visible in the button label and
requires a separate click for every bypass launch. It does not silently promote
a normal resume request, remember the dangerous choice, or fall back between
permission modes.

All existing launch validation remains mandatory:

- the session must exist;
- the source agent must be supported and configured;
- the native session ID must use the accepted character set;
- the working directory must be an existing absolute directory; and
- the source adapter must return the exact trusted normal resume specification.

Unsupported agents, invalid permission values, or a mismatch between the stored
agent and its resume specification fail closed. Missing cmux continues to
produce a copy response using the selected permission mode. A cmux execution
failure remains an error and is not converted into a second launch attempt.

## Testing Strategy

Development follows red-green-refactor behavior tests. Tests use fake runners
and never resume a real agent session.

Launcher tests cover:

- exact normal and bypass commands for Codex and Claude;
- exact cmux argv for both permission modes;
- copy fallback preserving the selected permission mode;
- rejection of unknown permission values;
- rejection of unsupported or mismatched source agents before flag insertion;
- preservation of the existing session ID, cwd, and quoting validation; and
- no mutation of the source adapter's normal arguments.

HTTP tests cover:

- forwarding `normal` and `bypass` to the launcher;
- defaulting an omitted `permissions` field to `normal`;
- rejecting an invalid permission value with no launcher call; and
- retaining the existing launch response contract.

Browser behavior tests cover:

- the two Codex labels and their request payloads;
- the two Claude labels and their request payloads;
- absence of an incompatible agent-specific bypass action;
- cmux success and copy-command fallback for both choices; and
- usable action layout at desktop and narrow viewport widths.

Repository verification includes formatting, focused package tests,
`go test ./...`, `go test -race ./...`, `go vet ./...`, and a production build.
Manual browser verification confirms both source-specific button sets and the
copy dialog's exact commands without executing either resume command.

## Documentation

The shipped design and implementation-plan resume sections will be updated to
describe both permission modes and the fixed API enum. The public README will
briefly document both explicit bypass actions and state that they disable the
corresponding vendor safeguards for the resumed process.

## Completion Criteria

The feature is complete when:

- every Codex and Claude session shows a normal and matching bypass resume
  action;
- each action produces the exact application-owned command for its agent;
- cmux and copy fallback preserve the selected permission mode;
- omitted permission mode remains normal for compatibility;
- no browser request can inject a command, executable, or CLI flag;
- automated tests prove the trusted normal specification is validated before
  bypass transformation; and
- the full repository verification suite and desktop/narrow UI checks pass
  without launching a real agent.

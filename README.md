# agent-history

`agent-history` is a local Go application for finding, understanding, and
resuming old Codex and Claude Code sessions. It stores normalized visible
conversation in SQLite, generates replaceable three-level summaries through the
Codex CLI, and serves a self-contained search interface from one binary.

## Install

Requirements:

- macOS;
- Go 1.24 or newer (the module selects a patched preferred toolchain);
- an authenticated Codex CLI for analysis; and
- optional `cmux` for one-click session launch, live session status, and title
  synchronization.

Build from this repository:

```sh
go build -trimpath -o agent-history ./cmd/agent-history
./agent-history version
```

No Node.js runtime, frontend build, external SQLite installation, or API key is
required.

## Run

Start the application:

```sh
./agent-history serve
```

By default it binds to a random port on `127.0.0.1`, prints and opens a
process-specific tokenized URL, scans histories at startup and every 15 minutes,
and stores data at `~/.local/share/agent-history/history.db`.

Useful alternatives:

```sh
# Print the URL without opening a browser.
./agent-history serve --no-open

# Disable automatic scans while retaining the Scan button.
./agent-history serve --scan-on-start=false --scan-interval=0

# Import without starting the server.
./agent-history scan --agent all
./agent-history scan --agent codex
./agent-history scan --agent claude
```

For the fixed local service used by this repository, run:

```sh
./run-local.sh
```

It serves directly at `http://127.0.0.1:54321/`, scans Codex and Claude history
at startup and every 15 minutes, and serially queues nonempty sessions that are
unanalyzed or have new activity. A failed analysis is retried once when the
process starts, but periodic scans do not repeatedly retry failures. Stop the
foreground process with `Ctrl-C`.

To start this fixed local service automatically after logging in to macOS:

```sh
./install-startup.sh
```

The idempotent installer creates and loads the per-user LaunchAgent
`com.tarunjain.agent-history`. It keeps the service running, uses the same
`run-local.sh` command, and writes output to
`~/Library/Logs/agent-history.log` and errors to
`~/Library/Logs/agent-history.error.log`. Running the installer again does not
create another job.

To stop the job and remove automatic startup:

```sh
./uninstall-startup.sh
```

The uninstaller is also idempotent. It does not delete the application,
configuration, imported conversations, summaries, or SQLite database.

Run `agent-history serve --help` for bind, database, config, browser, and scan
flags.

## Sources

The initial adapters discover Codex rollouts under `CODEX_HOME` or `~/.codex`,
and Claude Code project transcripts under `CLAUDE_CONFIG_DIR` or
`~/.claude/projects`. Nested Claude `subagents` transcripts are excluded.

The source transcript remains authoritative. Deleting SQLite and scanning again
reconstructs normalized messages deterministically from an unchanged source
snapshot. Generated summaries are replaceable cache data.

## Analysis

Select **Analyze** or **Reanalyze** in the session detail view. **Retitle** makes
one smaller model call over stored topic titles and summaries without rereading
the transcript or regenerating summaries. **Retitle weak titles** queues only
current titles that are short, generic, or duplicated. One worker serializes
requests and invokes authenticated `codex exec` in an ephemeral,
read-only temporary directory with schema-constrained output. The application
does not store Codex credentials or API keys.

Analysis consumes Codex subscription usage. Scanning and search do not invoke a
model. Claude Code histories are supported as sources, but this release does
not use a Claude Max subscription or invoke Claude for summaries.

Change the model in **Settings**. A new database may instead be seeded from:

```toml
# ~/.config/agent-history/config.toml
[analysis]
provider = "codex-cli"
model = "gpt-5.4-mini"
auto = true
```

After seeding, web settings take precedence. `auto` controls whether analysis
left queued or running by an unclean exit is requeued at startup; it does not
analyze every imported session automatically. The explicit `--analyze-pending`
serve flag enables that continuous local workflow and consumes Codex
subscription usage for newly queued work.

The summarizer uses content-addressed sealed leaves, chronological topic nodes,
and a session rollup. Appending conversation analyzes only new leaves and the
affected rollup path. Changing model, prompt version, or normalizer version
intentionally invalidates the applicable summary cache.

## Search And Actions

The embedded UI supports:

- full-text search over summaries, topics, visible messages, retained tool
  facts, errors, filenames, and working directories;
- agent, last-active preset, exact active/started range, folder, topic-count,
  analysis-state, and open-in-cmux filters;
- stable cursor pagination and last-active, started, or title sorting;
- start, last activity, total span, source path, and native session ID;
- overview, topic chapters, detailed evidence, and visible messages;
- Analyze, Reanalyze, Retitle, Retitle weak titles, Delete analysis, Rescan,
  Scan, Settings, Resume normally, and an agent-specific permission-bypass
  resume action; and
- keyboard result navigation and a responsive narrow layout.

Delete analysis preserves imported messages. Resume uses only validated stored
metadata. Codex sessions show **Resume normally** and **Resume with YOLO**;
Claude sessions show **Resume normally** and **Resume with dangerously skipped
permissions**. The bypass actions generate:

```text
codex resume --dangerously-bypass-approvals-and-sandbox <session-id>
claude --dangerously-skip-permissions --resume <session-id>
```

These actions disable the corresponding vendor safeguards for the resumed
process and must be chosen explicitly for each launch. The browser can select
only normal or bypass; it cannot submit flags or command text. `auto` launches
`cmux workspace create` when cmux is installed; otherwise the UI returns a
safely quoted resume command to copy.

## cmux Status And Title Sync

Agent History can match an imported Claude or Codex session to an open cmux
tab using cmux's native hook session ID. It does not infer matches from titles,
folders, or timestamps. Enable direct local socket access once on this Mac:

```sh
defaults write com.cmuxterm.app socketControlMode -string allowAll
defaults write com.cmuxterm.app workspaceAutoNamingEnabled -bool false
cmux capabilities --json
```

The capabilities response must report `"access_mode": "allowAll"`. Agent
History then refreshes cmux state at startup and every minute. The web page can
filter sessions by `Open in cmux`, shows the current workspace or exact tab
title beside the generated title, and provides an explicit **Send Agent
History title to cmux** action when they differ.

Open exactly matched Claude and Codex workspaces are colored from imported
transcript activity: Green through one hour, Orange after one hour and before
five hours, and Red at five hours or later. A shared workspace uses its most
recently active mapped agent. Terminal-only workspaces are not modified.
Transcript scans run every 15 minutes, so activity-color changes can lag by one
scan interval.

The header's **Auto refresh** checkbox controls only scheduled browser fetches
and is remembered by that browser. Turning it off also stops selected-analysis
polling; manual Refresh, background history scanning, analysis, and cmux color
reconciliation continue.

Automatic title sync is off by default. Enable **Automatically sync titles to
cmux** in Settings to opt in. Automatic sync writes only a current generated
title in a workspace containing one matched agent session, and it preserves a
cmux title changed independently. The explicit send action may replace a
different title; in a workspace with multiple matched sessions it renames only
the exact tab.

If cmux is closed, its socket is inaccessible, or access is not `allowAll`, the
Settings dialog reports cmux as unavailable. History scanning, analysis,
search, and browsing continue normally.

## Privacy And Security

- Private reasoning, thinking blocks, system/developer instructions,
  permission envelopes, and transport metadata are excluded before persistence,
  indexing, or analysis.
- Visible user/assistant text is retained verbatim. Tool commands and results
  have fixed retention limits for search and compaction.
- Transcript content is untrusted data and is never executed during ingestion.
- The server accepts only loopback binds, requires a random URL token by
  default, and validates Host and Origin on mutations. `--no-url-token`
  explicitly opts into a plain root URL while retaining the loopback bind.
- Normal logs contain paths, statuses, durations, and aggregate scan counts,
  not transcript text or request bodies.
- Settings reject secret-like keys and never return inherited credentials.
- cmux argv is generated from validated source metadata; the browser cannot
  submit an executable, arbitrary command, or resume flag.
- cmux socket paths and raw connection errors are not returned by the web API.

Individual transcripts are limited to 4 GB and individual JSONL records to 64
MB. These bounds accommodate the measured long-session corpus while preventing
unbounded reads. Scans and analysis each run with concurrency one.

## Backup And Rebuild

Use SQLite's online backup command while the server is running:

```sh
mkdir -p "$HOME/Backups/agent-history"
sqlite3 "$HOME/.local/share/agent-history/history.db" \
  ".backup '$HOME/Backups/agent-history/history.db'"
```

To restore, stop `agent-history`, replace the database with the backup, and
start the server. To rebuild imported history, stop the server, move or delete
the database, and run `agent-history scan --agent all`. Generated analysis must
then be recreated because transcripts do not contain it.

## Launch At Login

After placing the binary at an absolute path, create
`~/Library/LaunchAgents/local.agent-history.plist` with that path substituted:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>local.agent-history</string>
  <key>ProgramArguments</key>
  <array>
    <string>/absolute/path/to/agent-history</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>/tmp/agent-history.log</string>
  <key>StandardErrorPath</key><string>/tmp/agent-history.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/local.agent-history.plist"
```

## LLM Evaluation

Automated tests never invoke a real model. Quality evaluation is a separate,
explicitly acknowledged command over embedded sanitized multi-day fixtures:

```sh
./agent-history eval --model gpt-5.4-mini --allow-provider-usage
```

It reports title specificity, topic count, boundary F1, summary coverage,
evidence grounding, hidden-term exclusion, structured-output success, analyzer
calls, approximate input tokens, and append-only input growth. This command
invokes Codex repeatedly and consumes subscription usage.

## Measured Corpus

On 2026-07-11, a disposable scan imported 173 available sessions (26 Codex and
147 Claude), including a 1.31 GB Codex rollout, in 57.72 seconds. The normalized
SQLite/FTS database was about 560 MB. One hundred serial searches completed in
2.83 seconds; server-side durations were typically 21-23 ms. Active files can
change during measurement, so these are operational evidence, not guarantees.

## Development

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
go build ./cmd/agent-history
```

Design and acceptance details are in [docs/DESIGN.md](docs/DESIGN.md) and
[docs/IMPLEMENTATION_PLAN.md](docs/IMPLEMENTATION_PLAN.md). Deferred work
includes additional providers, embeddings, cross-session clustering, analysis
history, cloud synchronization, and a native cmux ExtensionKit frontend.

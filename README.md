# Agent History

Agent History is a local, set-it-and-forget-it search tool for Codex and Claude
Code sessions. It continuously imports new activity, builds useful titles and
three-level summaries, and keeps everything searchable in a self-contained
SQLite-backed web app.

## Highlights

- **Find old work quickly.** Search titles, hierarchical summaries, topics, or
  full conversations with AND matching. Narrow results by agent, working
  folder, activity or start date, single- or multi-topic session, analysis
  state, and whether the session is open in cmux.
- **Keep history current automatically.** The background service finds new and
  updated sessions, refreshes affected summaries, and keeps running after login
  or reboot.
- **Keep cmux organized.** Compare cmux and generated titles, send a better
  title to cmux manually, or enable automatic title sync. Active agent sessions
  also show live state and activity colors.
- **Resume where you stopped.** Open a Codex or Claude Code session directly in
  cmux, with normal and permission-bypassing resume options, or copy the resume
  command when cmux is unavailable.

Install it once, leave it running at
[http://127.0.0.1:54321/](http://127.0.0.1:54321/), and open it whenever you
need to recover an old conversation or continue unfinished work.

## Quick Start

### 1. Install and authenticate an analyzer

Install at least one provider. Agent History reuses the CLI's existing login and
never reads or stores credentials.

```sh
# Codex
npm install -g @openai/codex
codex login

# Claude Code (alternative or additional provider)
brew install --cask claude-code
claude auth login
```

Claude Code also supports its
[recommended native installer](https://code.claude.com/docs/en/getting-started).

### 2. Set up cmux

cmux is optional for search and analysis. Install it for live-session status,
title sync, activity colors, and one-click resume.

```sh
brew tap manaflow-ai/cmux
brew install --cask cmux
open -a cmux
cmux hooks setup
cmux hooks setup codex
```

See the [cmux project and current installation
instructions](https://github.com/manaflow-ai/cmux).

### 3. Allow direct cmux access

```sh
defaults write com.cmuxterm.app socketControlMode -string allowAll
defaults write com.cmuxterm.app workspaceAutoNamingEnabled -bool false
```

Restart cmux normally, then verify:

```sh
cmux capabilities --json
```

The response must report `"access_mode": "allowAll"`.

### 4. Install Agent History

```sh
git clone https://github.com/jaintarun/agent-history.git
cd agent-history
./install-startup.sh
```

The idempotent macOS installer builds the app, starts the per-user LaunchAgent,
and keeps it running after login and reboot. Run it again after pulling updates.

### 5. Open the app

Open [http://127.0.0.1:54321/](http://127.0.0.1:54321/), choose the analyzer and
model in **Settings**, then use **Scan** or wait for the background scanner.

## What It Does

- Discovers Codex rollouts under `CODEX_HOME` or `~/.codex`.
- Discovers Claude Code project transcripts under `CLAUDE_CONFIG_DIR` or
  `~/.claude/projects`; nested subagent transcripts are excluded.
- Removes private reasoning, thinking blocks, system/developer instructions,
  injected instruction envelopes, and transport metadata before persistence.
- Stores visible user/assistant text and bounded useful tool facts in SQLite.
- Generates a session title, overview, chronological topic summaries, and
  detailed topic evidence.
- Reuses sealed summaries when a multi-day session grows, analyzing only new
  conversation and affected rollups.
- Searches summaries and topics by default, with message fallback for
  unanalyzed sessions and unsummarized tails.
- Filters by agent, date ranges, folder, topic count, analysis state, and
  whether a session is open in cmux.
- Reanalyzes, retitles, deletes replaceable analysis, rescans, and resumes the
  original agent session.

Search terms use AND semantics. Enable **Include full conversations** to search
every retained normalized message; otherwise analyzed transcript text is not
searched, which keeps results focused.

## Analysis Providers

| Provider | Invocation | Default model | Authentication |
| --- | --- | --- | --- |
| Codex | `codex exec` | `gpt-5.4-mini` | Existing Codex CLI login |
| Claude Code | `claude -p` | `haiku` | Existing Claude Code login |

Provider executables are detected when the service starts. Install a missing
CLI, authenticate it, then rerun `./install-startup.sh`. Agent History does not
silently fall back to another provider.

Codex can use an eligible
[ChatGPT plan](https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan).
Claude Code supports Claude Pro, Max, Team, Enterprise, Console, and documented
third-party providers; follow
[Anthropic's authentication guidance](https://code.claude.com/docs/en/getting-started).
If `ANTHROPIC_API_KEY` is exported into the service environment, Claude Code may
use that key instead of browser-based subscription authentication. Agent
History does not implement login or route OAuth itself.

Analysis and title generation consume the selected provider's usage. Scanning,
filtering, search, cmux reconciliation, and browsing stored results do not call
a model.

For a new database, optional TOML configuration can seed the first selection:

```toml
# ~/.config/agent-history/config.toml
[analysis]
provider = "claude-cli" # or "codex-cli"
model = "haiku"
auto = true
```

Web settings take precedence after seeding. `auto` controls whether interrupted
queued/running jobs are requeued at startup. The fixed local service also uses
`--analyze-pending`, so newly imported or updated sessions are queued
continuously and serialized through one worker.

## Running Manually

Requirements are macOS and Go 1.24 or newer. The module selects a preferred
patched Go toolchain.

```sh
go build -trimpath -o agent-history ./cmd/agent-history
./agent-history serve
```

The generic `serve` command binds to a random loopback port, prints and opens a
tokenized URL, scans at startup and every 15 minutes, and stores data at
`~/.local/share/agent-history/history.db`.

The repository's fixed local command is:

```sh
./run-local.sh
```

It serves directly at `http://127.0.0.1:54321/`, scans every 15 minutes, and
queues pending analysis. Stop a foreground run with `Ctrl-C`.

Import without starting the server:

```sh
./agent-history scan --agent all
./agent-history scan --agent codex
./agent-history scan --agent claude
```

Run `./agent-history serve --help` for bind, database, config, browser, and scan
options.

## Startup Management

```sh
./install-startup.sh
./uninstall-startup.sh
```

The installer creates `io.github.jaintarun.agent-history`, migrates the legacy
LaunchAgent label if present, preserves paths containing spaces, and avoids
duplicate jobs. Logs are written to:

```text
~/Library/Logs/agent-history.log
~/Library/Logs/agent-history.error.log
```

The uninstaller is also idempotent. It stops automatic startup but keeps the
repository, configuration, imported conversations, summaries, and database.

## cmux Integration

Agent History matches open tabs only through native Codex or Claude session IDs;
it does not guess from titles, folders, or timestamps. When cmux access is
`allowAll`, the UI can:

- filter sessions open in cmux;
- compare cmux and generated titles;
- explicitly or automatically send generated titles to cmux;
- color mapped AI-agent workspaces by last transcript activity: green through
  one hour, orange after one and before five hours, red at five hours or later;
  and
- resume normally or with an explicit agent-specific permission bypass.

Codex shows **Resume normally** and **Resume with YOLO**. Claude shows **Resume
normally** and **Resume with dangerously skipped permissions**. Bypass actions
disable vendor safeguards and must be selected explicitly. Terminal-only cmux
workspaces are not recolored.

If cmux is closed or unavailable, importing, analysis, and search continue.

## Privacy and Security

- The source transcript remains authoritative; SQLite is a normalized local
  index plus replaceable generated analysis.
- Private reasoning and hidden instructions are excluded before storage,
  indexing, or model input.
- Transcript content is untrusted data and is never executed during ingestion.
- Analyzer calls run in temporary directories with no useful tools, bounded
  output, schema validation, timeouts, and no session persistence.
- The HTTP server accepts loopback binds only. Random URL tokens are the generic
  default; `run-local.sh` explicitly uses a plain loopback URL.
- API settings expose provider names and availability, never executable paths,
  inherited environment, keys, or credentials.
- Normal logs omit transcript text and request bodies.

See [SECURITY.md](SECURITY.md) for private vulnerability reporting.

## Backup and Rebuild

Back up the live database with SQLite's online backup command:

```sh
mkdir -p "$HOME/Backups/agent-history"
sqlite3 "$HOME/.local/share/agent-history/history.db" \
  ".backup '$HOME/Backups/agent-history/history.db'"
```

To rebuild, stop Agent History, move or delete the database, and scan again.
Generated analysis must be recreated because source transcripts do not contain
it.

## Development

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go build ./cmd/agent-history
```

Automated tests use fake provider executables and never consume model usage.
Quality evaluation is separate and explicitly acknowledged:

```sh
./agent-history eval --model gpt-5.4-mini --allow-provider-usage
```

See [CONTRIBUTING.md](CONTRIBUTING.md),
[docs/DESIGN.md](docs/DESIGN.md), and
[docs/IMPLEMENTATION_PLAN.md](docs/IMPLEMENTATION_PLAN.md).

## License

[MIT](LICENSE)

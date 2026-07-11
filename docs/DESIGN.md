# Agent History: High-Level Design

## Status

Proposed initial design. This document defines the smallest useful product and
the boundaries that should remain stable if a cmux or other frontend is added
later.

## Problem

Developers accumulate hundreds or thousands of long-running Codex and Claude
Code sessions. Existing transcript titles and literal text search are often not
enough to recover a conversation because:

- the remembered wording differs from the transcript;
- one session may cover several unrelated tasks;
- session files are distributed across provider-specific directories;
- raw transcripts include system instructions, reasoning, and large tool
  results that obscure the visible conversation; and
- useful metadata such as agent, working directory, start time, and last
  activity is not presented in one place.

## Product Goal

Provide one local interface where a developer can:

1. Search and filter Codex and Claude sessions.
2. Understand a session without reading its full transcript.
3. See when and where the work happened.
4. Reanalyze a session when its generated title or summary is poor.
5. Resume the original session when the source agent supports it.

## Design Principles

### Local source of truth

The agent's transcript remains authoritative. SQLite contains normalized
conversation data and replaceable analysis, not a second canonical transcript
format.

### Replaceable analysis

Generated titles, summaries, and topic segments may be deleted and recreated.
Reanalysis never deletes the normalized conversation.

### Retrieval before analytics

The application is a search and recovery tool, not a usage dashboard. Every
initial feature must improve finding, understanding, or resuming a session.

### Simple provider boundary

The first analyzer invokes the authenticated Codex CLI. Analysis code depends
on a small Go interface so a Claude CLI or HTTP provider can be added later.
There is one global default provider and model, with an optional per-request
override.

### No hidden execution

Transcript ingestion never executes transcript content. Analysis runs without
write access or useful tools. Session launch is an explicit user action and can
only invoke a resume specification created by a trusted source adapter.

## Scope

### Included

- Discover local Codex and Claude Code session transcripts.
- Normalize visible user text, visible assistant text, useful tool commands,
  and bounded tool results.
- Exclude reasoning, thinking blocks, system prompts, and injected instruction
  envelopes.
- Store session metadata and normalized messages in SQLite.
- Generate a session title, short summary, topic segments, and detailed segment
  summaries.
- Search with SQLite FTS5.
- Filter by agent, last-active date, custom date range, working directory,
  focused/multiple-topic status, and analysis state.
- Serve a self-contained web application from the Go binary.
- Analyze, reanalyze, delete analysis, rescan, copy a resume command, and launch
  through cmux when available.

### Not included initially

- Transcript backup or cloud synchronization.
- Embeddings, vector databases, or semantic reranking.
- Analysis version history or rollback.
- Estimated active working time.
- Multiple historical working directories per session.
- Separate models for segmentation, detail, rollup, and title.
- Cross-session topic clustering.
- A shared or multi-user server.
- Direct modification of cmux Vault.

## User Experience

### Startup

The normal entry point is:

```text
agent-history serve
```

The service binds to a random loopback port, creates an unguessable URL token,
and opens the default browser. `--no-open` prints the URL without opening it.
Static HTML, CSS, and JavaScript are embedded with `go:embed`; running the
application does not require Node.js or a separate frontend server.

### Primary view

The main screen is a dense search/detail split view:

```text
+-----------------------------------------------------------------------+
| Search sessions...  Agent v  Last active v  Folder v  More v          |
+--------------------------------+--------------------------------------+
| Results                        | Selected session                     |
|                                |                                      |
| Codex · Multiple · 3 topics    | Vault, extensions, and history      |
| Vault and history design       | Codex · ~/work/temp/cmux            |
| ~/work/temp/cmux               | Started / Last active / Span         |
| Last active 2 months ago       |                                      |
|                                | Short summary                        |
| Claude · Focused               |                                      |
| Fix payment migration          | Topic timeline                       |
| ~/work/payments                | 1. Vault investigation               |
| Last active 4 months ago       | 2. Extension analysis                |
|                                | 3. Go service design                 |
+--------------------------------+--------------------------------------+
```

Search and filters update the result list. Selecting a topic shows its detailed
summary and referenced messages. The conversation view shows normalized visible
messages and can optionally reveal retained tool activity.

### Session actions

The detail view exposes only the core actions:

- **Analyze**: create analysis for an unanalyzed session.
- **Reanalyze**: replace title, summary, and segments after a new analysis
  succeeds.
- **Delete analysis**: remove generated analysis while preserving messages.
- **Rescan**: reread the source transcript and update normalized messages.
- **Resume**: launch the original session or provide a copyable resume command.

## System Architecture

```text
 Codex files         Claude files
     |                    |
     +------ source adapters ------+
                                    |
                              normalizer
                                    |
                               SQLite/FTS5
                              /             \
                    analysis worker       search queries
                          |                      |
                    analyzer adapter          HTTP API
                          |                      |
                      Codex CLI          embedded web UI
                                                |
                                          launcher adapter
                                          /              \
                                        cmux         copy command
```

### Source adapters

Each source adapter owns provider-specific behavior:

```go
type Source interface {
    Name() string
    Discover(context.Context) ([]Candidate, error)
    Read(context.Context, Candidate) (ImportedSession, error)
    ResumeSpec(Session) (ResumeSpec, error)
}
```

The first adapters are:

- `codex`: active and archived rollout JSONL records under `CODEX_HOME` or
  `~/.codex`.
- `claude`: project JSONL records under `CLAUDE_CONFIG_DIR` or `~/.claude`.

Discovery records path, size, modification time, and native session ID. An
unchanged file is skipped. A changed file is parsed completely and its messages
are replaced in one transaction. Full-file replacement is intentionally simpler
than byte-offset tailing and correctly handles transcript rewrites.

### Normalization

The normalized message roles are:

```text
user
assistant
tool
```

The normalizer retains visible user and assistant text verbatim. Tool commands
and bounded tool output are retained to make errors, filenames, tests, and shell
commands searchable. Fixed internal size limits prevent single tool results
from dominating the database.

The normalizer discards:

- private reasoning or thinking blocks;
- system and developer instructions;
- environment and permission envelopes;
- empty events and transport metadata; and
- tool output beyond the fixed retention limit.

Filtering is verified with provider-specific fixtures so hidden content cannot
silently enter analysis prompts or search results.

### Analysis

The analyzer contract is intentionally small:

```go
type Analyzer interface {
    Analyze(
        context.Context,
        string, // model
        AnalysisInput,
    ) (AnalysisResult, error)
}
```

`AnalysisResult` contains:

```text
title
summary
topics[]
  title
  start_message
  end_message
  summary
  detail
```

The first implementation invokes `codex exec` using the user's existing Codex
login. It runs ephemerally in an empty temporary directory, uses a read-only
sandbox, receives the conversation through stdin, and validates the result with
a JSON Schema. The application does not extract or reuse Codex authentication
tokens.

For transcripts too large for one request, the service divides messages into
bounded chronological blocks, analyzes each block, then performs one merge call
over the candidate topics. All stages use the same configured model initially.

Reanalysis is atomic:

1. Generate and validate replacement analysis outside the write transaction.
2. Begin a transaction.
3. Replace the session title, summary, and segments.
4. Update the analyzed content hash and provenance.
5. Commit.

If generation fails, the previous analysis remains visible.

### Model switching

Configuration contains one default provider and model:

```toml
[analysis]
provider = "codex-cli"
model = "gpt-5.4-mini"
auto = true
concurrency = 1
```

The exact model name is configuration, not a compiled enum. A reanalysis
request may override provider or model. Analysis provenance is stored on the
session:

```text
analysis_provider
analysis_model
analysis_prompt_version
analyzed_at
analyzed_hash
```

The transcript source agent and analysis provider are separate. For example, a
Claude transcript may be summarized by `codex-cli`.

### SQLite model

The initial schema has four ordinary tables and one FTS5 virtual table.

#### `sessions`

```text
id                    internal stable ID
agent                 codex or claude
native_session_id     provider session ID
source_path           current transcript path
source_size           discovery fast-path
source_mtime          discovery fast-path
source_hash           normalized source identity
working_directory     primary cwd
title                 generated title, nullable
summary               generated short summary, nullable
topic_count           generated topic count, nullable
started_at            first meaningful event
last_active_at        last meaningful event
analysis_status       none, queued, running, complete, failed
analysis_error        last failure, nullable
analysis_provider     nullable
analysis_model        nullable
analysis_prompt_version nullable
analyzed_at           nullable
analyzed_hash         nullable
created_at
updated_at
```

`span` is computed as `last_active_at - started_at`; it is not stored as an
estimate of active working time.

#### `messages`

```text
session_id
sequence
timestamp
role
text
tool_name             nullable
```

#### `segments`

```text
session_id
position
start_sequence
end_sequence
title
summary
detail
```

#### `settings`

Stores non-secret application settings. Secrets remain in environment variables
or a later operating-system credential store.

#### `session_fts`

FTS documents cover generated titles and summaries, topic text, visible
messages, retained tool activity, and working directories. Results are grouped
by session. Ranking weights generated titles and topic titles above raw message
content.

### Search and filters

The supported query parameters are:

```text
q
agent
active_after
active_before
started_after
started_before
cwd
topic_mode            focused or multiple
analysis_status
sort                  last_active, started, title
cursor
limit
```

The UI supplies presets for 7 days, 30 days, 3 months, 6 months, 1 year, and all
time. A facets response supplies agent counts, common working directories,
analysis-state counts, and the available date range.

### Web API

```text
GET    /api/health
GET    /api/sessions
GET    /api/sessions/{id}
GET    /api/sessions/{id}/messages
GET    /api/sessions/facets

POST   /api/sessions/{id}/analyze
DELETE /api/sessions/{id}/analysis
POST   /api/sessions/{id}/rescan
POST   /api/sessions/{id}/launch

POST   /api/scan
GET    /api/settings
PUT    /api/settings
```

Analysis runs on one background worker. `sessions.analysis_status` is the queue
and status record; a separate job-management subsystem is not required.

### Embedded web application

The web application uses plain HTML, CSS, and browser JavaScript embedded with
`go:embed`. This avoids a frontend runtime and package-manager dependency while
still supporting:

- URL-backed search and filter state;
- paginated result loading;
- keyboard navigation through results;
- an accessible desktop split view and stacked narrow layout;
- topic expansion and message excerpts;
- analysis progress and errors; and
- explicit destructive-action confirmation.

The web frontend consumes only the HTTP API. A future cmux-native frontend can
use the same API without importing storage or analyzer code.

### Session resume and launch

Source adapters produce structured resume specifications rather than arbitrary
shell text:

```go
type ResumeSpec struct {
    Agent     string
    SessionID string
    CWD       string
    Executable string
    Args      []string
}
```

Initial specifications are equivalent to:

```text
codex resume <session-id>
claude --resume <session-id>
```

The launch endpoint supports two outcomes:

1. **cmux launch**: when configured and available, execute `cmux workspace
   create` with the session cwd and a generated resume command.
2. **Copy required**: return a safely rendered command for the browser to copy
   when no supported launcher is available.

`auto` chooses cmux when its executable is found and otherwise uses the copy
path. The API accepts only a session ID and launcher selection; it never accepts
an arbitrary executable or command from the browser. Native session IDs are
validated before command construction.

Launching a generic macOS terminal is deferred because it requires terminal-
specific automation and permissions. It can be added as another launcher
without changing the API or source adapters.

## Local Paths

Defaults:

```text
Database: ~/.local/share/agent-history/history.db
Config:   ~/.config/agent-history/config.toml
Logs:     stderr while foregrounded
```

All paths are configurable through flags for tests and alternate installations.

## Security and Privacy

- Bind only to `127.0.0.1` by default.
- Prefix UI and API routes with a random per-process token.
- Reject non-loopback hosts and unexpected browser origins.
- Use POST or DELETE for every state-changing action.
- Never expose provider credentials through settings responses.
- Never place API keys in SQLite.
- Treat transcripts as untrusted text and give analyzers no write authority.
- Validate structured analysis before persistence.
- Validate native session IDs and working directories before launch.
- Invoke subprocesses with argv, not a shell, except for the cmux terminal
  command text generated solely from validated internal fields.

## Operational Behavior

- Scan on startup.
- Scan on a configurable interval while serving.
- Allow manual global scan and per-session rescan.
- Use a single analysis worker by default to protect subscription usage.
- Recover sessions left in `queued` or `running` state after an unclean exit by
  returning them to `queued` or `none` according to configuration.
- Log source, session ID prefix, operation, duration, and error without logging
  transcript content.

## Success Criteria

The first release is successful when a developer can:

1. Import representative Codex and Claude histories without storing reasoning.
2. Find an old session by exact text, generated concepts, agent, date, or folder.
3. distinguish focused and multiple-topic sessions from the result list.
4. Inspect title, summary, topic details, original visible messages, start time,
   last activity, and span.
5. Reanalyze a session without losing the previous result on failure.
6. Delete analysis without deleting messages.
7. Resume through cmux or copy a valid resume command.
8. Run the binary and web UI without Node.js or external static assets.

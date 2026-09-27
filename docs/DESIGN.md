# Agent History: High-Level Design

## Status

Implemented for the first local release. This document defines the shipped
product and the boundaries that should remain stable if a cmux or other
frontend is added later.

## Problem

Developers accumulate hundreds or thousands of long-running Codex, Claude Code,
and Grok sessions. Existing transcript titles and literal text search are often
not enough to recover a conversation because:

- the remembered wording differs from the transcript;
- one session may cover several unrelated tasks;
- session files are distributed across provider-specific directories;
- raw transcripts include system instructions, reasoning, and large tool
  results that obscure the visible conversation; and
- useful metadata such as agent, working directory, start time, and last
  activity is not presented in one place.

## Product Goal

Provide one local interface where a developer can:

1. Search and filter Codex, Claude Code, and Grok sessions.
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

The shipped analyzers invoke an authenticated Codex CLI or Claude Code CLI
through the same small Go interface. There is one global provider and model
selection, with an optional per-request override. Executable discovery is fixed
and explicit; an unavailable provider never silently falls back to another.

### Incremental analysis

Long-running sessions must not be summarized from the beginning whenever new
messages arrive. Raw conversation is summarized into sealed, content-addressed
nodes. An append analyzes only the new conversation and recomputes the affected
path through the summary tree. Complete reanalysis remains an explicit user
action or a consequence of changing the model, prompt, or normalizer version.

### No hidden execution

Transcript ingestion never executes transcript content. Analysis runs without
write access or useful tools. Session launch is an explicit user action and can
only invoke a resume specification created by a trusted source adapter.

## Scope

### Included

- Discover local Codex, Claude Code, and Grok session transcripts.
- Normalize visible user text, visible assistant text, useful tool commands,
  and bounded tool results.
- Exclude reasoning, thinking blocks, system prompts, and injected instruction
  envelopes.
- Store session metadata and normalized messages in SQLite.
- Generate a session title, short summary, topic segments, and detailed segment
  summaries.
- Incrementally reuse sealed summaries as sessions continue over multiple days.
- Search with SQLite FTS5.
- Filter by agent, last-active date, custom date range, working directory,
  focused/multiple-topic status, and analysis state.
- Serve a self-contained web application from the Go binary.
- Observe exactly matched live cmux sessions and optionally synchronize the
  generated title and short summary as workspace metadata.
- Analyze, reanalyze, delete analysis, rescan, and resume normally or with an
  explicit agent-specific permission bypass through cmux or a copyable command.

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

For a trusted single-user machine, an explicit `--no-url-token` mode may mount
the same application at `/`; loopback binding, Host validation, and mutation
Origin validation remain mandatory. `--analyze-pending` queues nonempty new or
updated sessions after scans while keeping model work serialized.

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
- **Retitle**: generate one catalog title from stored topic titles and summaries
  without rereading the transcript or replacing other analysis.
- **Retitle weak titles**: queue title-only jobs for short, generic, or duplicate
  current titles.
- **Delete analysis**: remove generated analysis while preserving messages.
- **Rescan**: reread the source transcript and update normalized messages.
- **Resume normally**: launch the original session with the source CLI's normal
  permission behavior or provide a copyable command.
- **Resume with bypass**: explicitly launch Codex with
  `--dangerously-bypass-approvals-and-sandbox` or Claude with
  `--dangerously-skip-permissions`, or Grok with `--always-approve`.

## System Architecture

```text
 Codex files       Claude files       Grok files
      |                 |                 |
      +---------- source adapters --------+
                       |
                  normalizer
                       |
                  SQLite/FTS5
                 /             \
       analysis worker       search queries
             |                      |
 turn builder + compactor          HTTP API
             |                      |
 boundary scorer + summary tree     |
             |                      |
      analyzer adapters      embedded web UI
             |                      |
     Codex / Claude CLI       launcher adapter
                                  /       \
                                cmux   copy command
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
  `~/.codex`. Session metadata marked as a spawned or review subagent is
  excluded; a later scan removes previously imported child sessions.
- `claude`: project JSONL records under `CLAUDE_CONFIG_DIR` or `~/.claude`.
- `grok`: main ACP update streams under `GROK_HOME` or `~/.grok/sessions`.
  Grok stores child histories beside main sessions, so streams whose metadata
  marks them as subagents and events removed by rewind markers are not imported.

Discovery records path, size, modification time, and native session ID. An
unchanged file is skipped. A changed file is parsed completely and its messages
are replaced in one transaction. Full-file replacement is intentionally simpler
than byte-offset tailing and correctly handles transcript rewrites.

Before replacement, the importer compares normalized message hashes to find the
longest unchanged prefix. A normal append preserves every sealed summary node
whose range is inside that prefix. A rewrite invalidates nodes overlapping the
changed suffix plus rollup ancestors that depend on them. This keeps local
ingestion simple while avoiding repeated model work for unchanged history.

### Normalization

The normalized message roles are:

```text
user
assistant
tool
```

The normalizer retains visible user and assistant text verbatim. Codex and
Claude retain bounded tool commands and output. Grok retains only the main
conversation; its tool calls and results are neither stored nor indexed.
Fixed internal size limits prevent retained tool results from dominating the
database.

The normalizer discards:

- private reasoning or thinking blocks;
- system and developer instructions;
- environment and permission envelopes;
- empty events and transport metadata; and
- tool output beyond the fixed retention limit.

Filtering is verified with provider-specific fixtures so hidden content cannot
silently enter analysis prompts or search results.

When a Grok session stored by the earlier tool-inclusive normalizer is
rescanned, the new normalization version forces one reimport even if the source
file is unchanged. If normalized messages change, the import atomically removes
tool records, their FTS rows, and analysis generated from the old transcript.
When automatic analysis is enabled, it then rebuilds summaries from the
conversation. Sessions whose normalized messages do not change keep their
existing analysis.

### Turns and analysis projection

The analysis worker groups normalized records into natural turns: one user
request, the visible assistant response around it, related tool activity, and
the resulting assistant conclusion. Topic boundaries are never created merely
because midnight or an idle timeout occurred.

The worker then builds a compact analysis projection without changing the
stored conversation:

- preserve meaningful user messages and visible assistant text;
- reduce tool calls to command, file, and exit-status facts;
- keep error lines with bounded surrounding context;
- reduce diffs to files and change statistics unless their text is directly
  discussed;
- collapse repeated log lines and identical content; and
- replace repeated material with a count and content reference.

SQLite and FTS retain the normalized visible text. The compact projection is
used only as model input, so token reduction does not weaken exact transcript
search.

### Analysis

The analyzer contract is intentionally small:

```go
type Analyzer interface {
    Generate(
        context.Context,
        string, // model
        StructuredRequest,
    ) (json.RawMessage, error)
}
```

`StructuredRequest` supplies the bounded prompt input and JSON Schema. The
analysis pipeline, not the provider adapter, decodes that output into typed leaf
summaries, boundary decisions, topic rollups, or session rollups. This keeps
Codex-, Claude-, and HTTP-specific authentication and process behavior separate
from summary-tree policy.

The user-visible `AnalysisResult` contains:

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

`codex-cli` invokes `codex exec` ephemerally in an empty temporary directory
with a read-only sandbox. `claude-cli` invokes `claude -p` with no tools, no
session persistence, one turn, and structured JSON output. Both receive the
conversation through stdin, reuse the installed CLI's existing authentication,
bound stdout/stderr and runtime, and validate schema-constrained output. The
application does not extract or store provider credentials.

#### Sealed leaf blocks

Compacted turns accumulate into bounded leaf blocks sized for the configured
model. A leaf is sealed when it reaches its input target, a strong topic boundary
is detected, the session becomes idle, or the source session ends. A continuously
running agent therefore seals by size even when it never becomes idle.

Each sealed leaf records its message range, input hash, compact structured
summary, entities, goal, outcome, important files and errors, and analysis
provenance. Sealed leaves are immutable while their input hash and configuration
remain unchanged. In normal operation, each raw message is included in at most
one sealed leaf analysis, apart from a small overlap used to preserve context.

The current tail remains unsealed. The service does not invoke the selected
analyzer after every message. It analyzes the tail after a size threshold, an
idle period, an explicit request for fresh analysis, or a strong boundary. The
API reports how far the analysis is current so the UI can distinguish fresh
analysis from new queued conversation.

#### Topic boundary detection

Boundary detection is local by default. It scores evidence such as:

- an explicit new goal in a user message;
- completion of the previous objective;
- a repository, cwd, or branch change;
- a substantial shift in files, commands, named entities, or vocabulary; and
- an idle gap combined with another topic-shift signal.

An idle gap alone never splits a topic. A two-day goal-oriented task may remain
one topic, while a session reused for unrelated work becomes multiple
chronological topics.

Only ambiguous boundaries use the analyzer. That request contains the current
topic title and goal plus small excerpts immediately before and after the
candidate boundary. It returns a same-topic decision, confidence, and optional
new topic title rather than re-reading the transcript.

#### Incremental summary tree

Sealed leaves roll into user-visible topic nodes. Topic summaries roll into the
session title and overview. Very large topics may use fixed-fanout intermediate
rollup nodes, but those nodes are internal and do not add UI hierarchy.

A title-only refinement may run after the hierarchy is complete. It receives
only session metadata plus topic titles and summaries, targets a specific
8-16-word catalog title, and atomically updates the session title, root node,
and FTS document. Failure preserves the previous title and summaries. Retitle
jobs share the serialized analysis worker.

```text
Session overview
├── Authentication topic
│   ├── sealed leaf: reproduce failure
│   ├── sealed leaf: inspect token refresh
│   └── sealed leaf: implement and test fix
├── Vault research topic
│   ├── sealed leaf: inspect storage
│   └── sealed leaf: evaluate extension paths
└── History-service topic
    ├── sealed leaf: define data model
    └── active leaf: design filtering
```

Appending one leaf normally requires one leaf analysis, one affected-topic
rollup, and one session rollup. Only the leaf request contains new raw
conversation; parent requests contain compact child summaries. Work grows with
the new conversation and the changed tree path rather than total session size.

The visible hierarchy remains exactly three generated levels:

1. session title and overview;
2. chronological topic chapters; and
3. detailed topic summaries with evidence message references.

If work returns to an earlier subject after unrelated chapters, create and title
a new chronological follow-up topic rather than rewriting history.

Reanalysis is atomic:

1. Generate and validate replacement nodes outside the user-visible write
   transaction.
2. Begin a transaction.
3. Replace the session title, summary, segments, and active analysis generation.
4. Update the analyzed content hash and provenance.
5. Commit and remove superseded internal nodes.

If generation fails, the previous analysis remains visible.

Normal appends use the incremental path. Explicit reanalysis with a different
model, prompt version, or normalizer version intentionally rebuilds the tree.
Deleting analysis removes segments and summary nodes but preserves messages.

### Model switching

An optional TOML configuration seeds a new database with one provider and
model:

```toml
[analysis]
provider = "codex-cli"
model = "gpt-5.6-luna"
auto = true
```

The alternate shipped selection is `provider = "claude-cli"` with model
`"haiku"`. Provider IDs are fixed; exact model names are configuration rather
than a compiled enum. Once seeded, web settings stored in SQLite take
precedence. A reanalysis request may override the model. Analysis provenance is
stored on the session:

```text
analysis_provider
analysis_model
analysis_prompt_version
analyzed_at
analyzed_hash
```

The transcript source agent and analysis provider are separate. Either shipped
analyzer CLI may summarize a Codex, Claude Code, or Grok transcript. Grok is a
session source and resume target, not an analysis provider.

Leaf-size, overlap, idle, boundary-confidence, and rollup-fanout values start as
tested internal defaults rather than user-facing knobs. Promote one to a setting
only when real histories demonstrate a need to tune it.

### SQLite model

The initial schema has five ordinary tables and one FTS5 virtual table.

#### `sessions`

```text
id                    internal stable ID
agent                 codex, claude, or grok
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
analysis_status       none, queued, running, current, partial, failed
analysis_error        last failure, nullable
analysis_provider     nullable
analysis_model        nullable
analysis_prompt_version nullable
analyzed_at           nullable
analyzed_hash         nullable
analyzed_through_sequence nullable
analyzed_through_at   nullable
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

`segments` are the topic chapters shown to users. Internal leaf and rollup nodes
do not appear as additional topics.

#### `summary_nodes`

```text
id
session_id
parent_id             nullable
kind                  leaf, rollup, topic, session
position
start_sequence
end_sequence
input_hash
summary_json
sealed
provider
model
prompt_version
normalizer_version
created_at
updated_at
```

The effective cache identity is the input hash plus provider, model, prompt
version, and normalizer version. Nodes whose cache identity and message range
remain valid are reused. This table is an optimization structure, not analysis
history: superseded nodes are deleted after a successful replacement.

#### `settings`

Stores non-secret application settings. Secrets remain in environment variables
or a later operating-system credential store.

#### `session_fts`

FTS documents cover generated titles and summaries, topic text, visible
messages, retained tool activity, and working directories. Results are grouped
by session. Default keyword search admits generated session and topic documents,
all normalized messages for sessions without stored analysis, and only messages
after the analyzed-through position for partial sessions. An explicit
`include_messages=true` scope admits every normalized message. Working
directories remain materialized for rebuild compatibility but are queried
through the dedicated folder filter rather than keyword matching.

### Search and filters

The supported query parameters are:

```text
q
include_messages      true includes every normalized message; false or absent is focused
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
time. **Include full conversations** controls the explicit message scope and is
stored in the URL, not application settings. A facets response supplies agent
counts, common working directories, analysis-state counts, and the available
date range.

### Web API

```text
GET    /api/health
GET    /api/sessions
GET    /api/sessions/{id}
GET    /api/sessions/{id}/messages
GET    /api/sessions/facets

POST   /api/sessions/{id}/analyze
POST   /api/sessions/{id}/retitle
DELETE /api/sessions/{id}/analysis
POST   /api/sessions/{id}/rescan
POST   /api/sessions/{id}/launch

POST   /api/scan
POST   /api/retitle-weak
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
- focused hierarchy search with an explicit full-conversation checkbox;
- paginated result loading;
- one-minute in-place data refresh with a manual countdown control that
  preserves current filters and selection;
- keyboard navigation through results;
- an accessible desktop split view and stacked narrow layout;
- topic expansion and message excerpts;
- analysis-current-through status when a live session has an unsealed tail;
- analysis progress and errors; and
- explicit destructive-action confirmation.

The web frontend consumes only the HTTP API. A future cmux-native frontend can
use the same API without importing storage or analyzer code.

### cmux metadata synchronization

Agent History matches live cmux tabs by source agent and native session ID. For
a workspace containing one mapped agent session, the generated session title
is the workspace and tab title, and the existing generated short summary is the
workspace description. This reuses current analysis and never makes a separate
model call for cmux metadata.

Automatic synchronization writes only blank metadata or metadata that still
equals the last value Agent History successfully pushed. A manual change in
cmux is therefore preserved until the user explicitly sends Agent History
metadata again. A shared workspace can receive per-tab titles, but it receives
no per-session description because cmux descriptions are workspace-level.

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

Source adapter specifications remain equivalent to the normal commands:

```text
codex resume <session-id>
claude --resume <session-id>
grok --resume <session-id>
```

After validating that normal specification against stored metadata, the
launcher may apply the fixed `bypass` transform:

```text
codex resume --dangerously-bypass-approvals-and-sandbox <session-id>
claude --dangerously-skip-permissions --resume <session-id>
grok --always-approve --resume <session-id>
```

The launch endpoint supports two outcomes:

1. **cmux launch**: when configured and available, execute `cmux workspace
   create` with the session cwd and a generated resume command.
2. **Copy required**: return a safely rendered command for the browser to copy
   when no supported launcher is available.

`auto` chooses cmux when its executable is found and otherwise uses the copy
path. The API accepts only a session ID, `auto|cmux|copy` launcher selection,
and `normal|bypass` permission selection. An omitted permission selection
defaults to `normal`. It never accepts an arbitrary executable, command, or flag
from the browser. Native session IDs, working directories, and the exact normal
source specification are validated before command construction or bypass flag
insertion.

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
- Seal analysis leaves by size or idle threshold rather than summarizing every
  message.
- Reuse unchanged summary nodes and recompute only the affected tree path.
- Recover sessions left in `queued` or `running` state after an unclean exit by
  returning them to `queued` or `none` according to configuration.
- Log aggregate scan counts and HTTP operation/status/duration without logging
  transcript content or request bodies.

## Success Criteria

The first release is successful when a developer can:

1. Import representative Codex, Claude Code, and Grok histories without storing
   reasoning or abandoned Grok rewind branches.
2. Find an old session by exact text, generated concepts, agent, date, or folder.
3. distinguish focused and multiple-topic sessions from the result list.
4. Inspect title, summary, topic details, original visible messages, start time,
   last activity, and span.
5. Reanalyze a session without losing the previous result on failure.
6. Delete analysis without deleting messages.
7. Resume normally or with the matching agent-specific permission bypass
   through cmux or a valid copyable command.
8. Run the binary and web UI without Node.js or external static assets.
9. Append new conversation to a multi-day session without re-sending sealed
   history to the analyzer.
10. Keep an overnight continuation in one topic while detecting a genuine task
    change as a new chronological topic.

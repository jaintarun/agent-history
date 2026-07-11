# agent-history

`agent-history` is a local Go service for finding and understanding old Codex
and Claude Code sessions.

It scans local session transcripts, removes non-visible reasoning content,
stores the visible conversation in SQLite, generates a three-level analysis,
and provides full-text search through an embedded web application. A session
can be reanalyzed at any time, and supported sessions can be resumed from the
web UI.

This repository is currently documentation-first. Implementation has not
started.

## Documents

- [High-level design](docs/DESIGN.md)
- [Detailed implementation plan](docs/IMPLEMENTATION_PLAN.md)

## Initial scope

- Local Codex and Claude Code transcripts
- SQLite storage with FTS5
- Codex CLI as the first analysis provider
- Search and filtering by text, agent, date, folder, topic count, and analysis
  state
- Embedded, self-contained web UI served by the Go binary
- Analyze, reanalyze, delete analysis, rescan, and resume actions
- Optional cmux launcher integration

## Deferred

- Cloud synchronization
- Embedding or vector search
- Historical versions of generated analysis
- Per-analysis-stage model routing
- Cross-session topic clustering
- cmux ExtensionKit integration

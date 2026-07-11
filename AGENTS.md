# AGENTS.md

## Mission

Implement `agent-history` as the smallest useful local application described in
`docs/DESIGN.md`, following the phases and exit conditions in
`docs/IMPLEMENTATION_PLAN.md`. Read both documents before changing code.

## Working Method

- Work in vertical, testable increments and keep the repository buildable after
  every phase.
- For each behavior, write a failing behavior-level test, confirm the failure,
  implement the minimum code, and rerun the focused and repository checks.
- Keep changes within the current implementation phase. Do not combine unrelated
  phases or add deferred features.
- Prefer concrete Go types. Add interfaces only at external boundaries such as
  transcript sources, analyzers, launchers, clocks, or command runners.
- Preserve user changes and inspect `git status` and the relevant diff before
  committing.
- Never use a real model or spend subscription/API usage in automated tests.

## Product Boundaries

- Use Go, SQLite with FTS5, and HTML/CSS/minimal browser JavaScript embedded with
  `go:embed`. Produce one self-contained executable with no Node.js toolchain.
- The source transcript is authoritative. SQLite stores normalized visible
  messages and replaceable generated analysis.
- Keep deferred work deferred: no embeddings, cloud sync, analysis history,
  cross-session clustering, per-stage model routing, generic terminal launchers,
  or direct cmux Vault integration without an explicit design change.
- Treat leaf sizing, overlap, idle thresholds, boundary confidence, and rollup
  fanout as tested internal defaults until real histories prove a user setting is
  needed.

## Non-Negotiable Invariants

- Never persist, index, log, or send private reasoning, thinking blocks, system
  or developer instructions, permission envelopes, or transport metadata to an
  analyzer.
- Transcript ingestion never executes transcript content. Analyzers receive no
  write authority or useful tools.
- Reanalysis is atomic: a failure preserves the last successful visible
  analysis. Deleting analysis preserves normalized messages.
- Re-importing unchanged sources is idempotent and does not duplicate messages.
- Appending a session reuses valid sealed nodes and processes only the new leaf
  and affected tree path. Rewriting earlier content invalidates only overlapping
  nodes and their ancestors.
- Generated analysis has exactly three visible levels: session overview,
  chronological topic chapters, and detailed topic summaries with evidence
  references.
- Source agent and analysis provider remain separate concepts. Provider, model,
  prompt version, normalizer version, and input hash are analysis provenance.
- SQLite mutations that span related records are transactional. Enable foreign
  keys on every connection, keep writes short, and use parameterized SQL.
- Bind HTTP to loopback by default, require the per-process URL token, validate
  origins for mutations, and never expose credentials or transcript text in
  normal logs.
- Launch requests select only trusted stored resume specifications. Validate
  session IDs and working directories, invoke subprocesses with argv, and never
  accept an arbitrary browser-supplied command or executable.

## Tests

- Use sanitized provider-specific golden fixtures for transcript normalization.
- Use temporary real SQLite databases for migrations, FTS5, transactions, and
  concurrency behavior; do not mock SQL.
- Use `httptest` for the API and fake executables for Codex, Claude, and cmux.
- Inject clocks, IDs, analyzers, and command runners where deterministic tests
  require them.
- Test malformed and partial transcripts, invalid analyzer output, cancellation,
  timeouts, nonzero exits, and analyzer failure without data loss.
- Test incremental-summary invariants and token/input growth independently of
  generated prose. Exact model prose does not belong in deterministic tests.
- Fuzz parsers, path handling, and structured analyzer-output decoding once
  those surfaces exist.

## Verification

Run the checks applicable to the current phase before declaring it complete:

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
go build ./cmd/agent-history
```

Also verify each phase's explicit checks and exit condition in
`docs/IMPLEMENTATION_PLAN.md`. For web UI work, start the server and verify the
documented workflows in desktop and narrow browser viewports. If a check cannot
run, report the exact reason; do not silently omit it or weaken a valid test.

## Completion Standard

Work is complete only when the relevant implementation-plan exit condition is
met with verification evidence, the diff contains only intentional changes, and
the documentation still matches the implemented behavior.

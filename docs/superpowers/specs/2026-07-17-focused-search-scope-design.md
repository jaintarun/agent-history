# Focused Search Scope Design

**Date:** 2026-07-17
**Status:** Design approved; pending written-spec review

## Purpose

Reduce noisy search results by making generated hierarchical analysis the
default searchable text for analyzed sessions. Preserve discoverability for
sessions and recent conversation that do not yet have generated analysis, while
keeping full normalized conversation search available through one explicit
checkbox.

The current FTS index contains one session document, one document per generated
topic, and one document per normalized message. Weighting generated text above
messages changes ranking but still admits every transcript-only match. The new
behavior changes which document types may admit a session instead of trying to
tune transcript matches lower.

## User Experience

The search bar gains an unchecked **Include full conversations** checkbox.

With the checkbox off, focused search uses these rules:

- A session with stored generated analysis searches its generated session title,
  overview, topic titles, topic summaries, and topic details.
- A session with no stored generated analysis searches its normalized visible
  conversation and retained tool text.
- A session with stored generated analysis and newer imported messages searches
  its generated hierarchy plus only the messages after
  `analyzed_through_sequence`.

With the checkbox on, search uses the generated hierarchy and every normalized
visible message for every session.

The current multi-word AND behavior remains unchanged. A session must satisfy
all search terms within one eligible FTS document. Checking the box broadens
the eligible documents; it does not change terms to OR.

The default placeholder becomes **Search summaries and topics**. When full
conversation search is enabled, it becomes **Search summaries and
conversations**. No explanatory banner, result badge, modal, or settings page
is added.

## Analysis-State Semantics

Search scope is based on whether a successful generated analysis is stored, not
only on the transient `analysis_status`.

`analysis_provider IS NOT NULL` identifies a stored successful analysis because
analysis provenance is written atomically with the title, overview, topics, and
analyzed-through position. This correctly handles:

- `none` after analysis deletion: search the normalized conversation;
- first-time `queued`, `running`, or `failed`: search the normalized
  conversation because no successful analysis exists;
- reanalysis `queued`, `running`, or `failed`: search the last successful
  hierarchy and any unsummarized tail;
- `current`: search only the generated hierarchy unless full conversations are
  enabled; and
- `partial`: search the hierarchy plus messages whose sequence is greater than
  `analyzed_through_sequence`.

The existing atomic reanalysis invariant is preserved. A failed reanalysis
continues to expose and search the previous successful hierarchy.

## Searchable Content

Focused keyword search uses only the FTS `title` and `body` columns:

- `session` documents contain the generated title and overview;
- `segment` documents contain topic titles, summaries, and details; and
- eligible `message` documents contain normalized visible conversation or
  retained tool names and text.

The `working_directory` FTS column no longer participates in keyword matching
in either scope. The dedicated working-folder filter remains available and
continues to compose with keyword search. Filenames and paths present in
generated analysis, visible conversation, commands, or retained tool output
remain searchable through their document text.

Private reasoning, thinking blocks, system/developer instructions, permission
envelopes, transport metadata, and truncated tool content remain excluded
because search continues to use the normalized message store. The checkbox
does not expose or re-ingest source transcript data.

## Store And FTS Query

The existing `session_fts` table and documents remain unchanged. No migration,
rebuild, or duplicate index is required.

`store.SearchQuery` gains:

```go
IncludeMessages bool
```

Both tokenized FTS matching and punctuation-only literal fallback apply the same
document eligibility rules. This prevents punctuation searches from silently
returning the full transcript when focused search is selected.

For a nonempty query, eligible documents are:

```text
session or segment
OR include_messages is true and document_type is message
OR document_type is message and the session has no successful analysis
OR document_type is message and its sequence is after analyzed_through_sequence
```

The FTS query is column-scoped to `title` and `body`. The implementation joins
FTS rows to `sessions` for analysis provenance and analyzed-through data. Message
sequence is obtained from the internally generated `message:<sequence>`
document key; browser input never controls document keys.

The best eligible row per session continues to supply its snippet and score.
Snippets therefore describe a hierarchy match, an unanalyzed-session message,
or an unsummarized-tail message in focused mode. Existing relevance sorting,
explicit sorts, filters, cursor pagination, and result response fields remain
unchanged.

The checkbox has no effect when the text query is empty. All sessions still
participate in filter-only browsing.

## HTTP Contract

`GET /api/sessions` gains:

```text
include_messages=true
```

The parameter is absent by default. The API accepts only an absent value,
`true`, or `false`; any other supplied value returns `400 invalid_query`.
`false` is equivalent to omission.

This is an intentional default behavior change for API clients: a text query
without the parameter performs focused search. Clients that require the former
all-document behavior must send `include_messages=true`.

## Web State And Layout

The checkbox is placed directly beside the search field inside the search area.
Its compact label is **Include full conversations**. It uses a normal checkbox,
not a settings toggle or menu item.

The browser:

- adds `include_messages=true` to search requests only while checked;
- writes the same parameter to the page URL;
- restores the checkbox from the URL after reload;
- preserves it across automatic refresh, manual refresh, pagination, and
  session selection;
- removes it when the user unchecks the box; and
- unchecks it when **Clear all** is used.

The state is not stored in `localStorage`, SQLite settings, or server
configuration. A new page without the URL parameter always starts in focused
mode.

The search control and checkbox must fit without overflow at desktop and narrow
widths. At constrained widths they may wrap within the search area rather than
compressing the text input below a usable width.

## Alternatives Rejected

### Separate summary and transcript FTS tables

This gives simple queries but duplicates schema, indexing, rebuild, and mutation
logic. The existing `document_type` and session metadata already support the
required scope safely.

### Ranking or score thresholds

Changing BM25 weights or dropping low-scoring rows does not solve the admission
problem. Transcript-only matches would still appear unpredictably and threshold
tuning would vary with corpus size and term frequency.

### Manual second search

Searching summaries first and requiring a separate retry for transcripts is
quiet but hides unanalyzed sessions and recent unsummarized work. The automatic
fallback rules retain those cases without reopening old analyzed transcripts.

## Error Handling

- Invalid `include_messages` values fail before the store query.
- FTS syntax failures continue to use literal fallback with the same scope.
- Search cancellation and stale-result suppression remain unchanged.
- If a session's message document key is malformed, it cannot qualify as an
  unsummarized-tail document; ordinary hierarchy and explicit full-message
  search still behave normally.
- Search never triggers analysis automatically.

## Testing Strategy

Development follows red-green-refactor behavior tests.

Store tests cover:

- transcript-only terms are excluded for analyzed sessions by default;
- the same terms match when `IncludeMessages` is true;
- generated title, overview, and topic terms match by default;
- a session with no successful analysis matches its message text by default;
- a partial session matches only messages after `analyzed_through_sequence`;
- messages already covered by the hierarchy do not match by default;
- first-analysis and reanalysis queued/running/failed states use stored
  provenance rather than status alone;
- working-directory-only terms do not match keyword search in either scope;
- the working-folder filter remains functional;
- tokenized and literal fallback paths enforce identical scope; and
- AND semantics, scoring, explicit sorts, filters, and pagination remain green.

HTTP tests cover absent, `true`, `false`, and invalid `include_messages` values
and forwarding to the store query.

Browser behavior tests cover checkbox request payload, URL restoration,
unchecking, clear-all behavior, placeholder text, refresh preservation, and
pagination preservation.

Responsive verification covers desktop, the existing intermediate-width
boundary, and approximately 390 CSS pixels. It confirms the search field remains
usable and the checkbox does not overlap filters or force horizontal overflow.

The complete repository verification remains `go test ./...`,
`go test -race ./...`, `go vet ./...`, `govulncheck ./...`, JavaScript syntax
checks, and a production build. Tests never invoke an analyzer.

## Completion Criteria

The feature is complete when:

- default text search excludes historical transcript-only matches from sessions
  with successful analysis;
- unanalyzed sessions and only the unsummarized tail of partial sessions remain
  discoverable by default;
- checking **Include full conversations** restores all normalized-message
  matching;
- keyword search excludes working-directory-only matches while the folder
  filter continues to work;
- checkbox and selection/filter state survive refresh through URL-backed state;
- snippets come only from documents eligible for the selected scope;
- no migration, reindex, or new persistent setting is introduced; and
- automated and responsive verification pass without analyzer usage.

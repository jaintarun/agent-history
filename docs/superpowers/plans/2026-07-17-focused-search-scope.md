# Focused Search Scope Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make analyzed sessions search their generated hierarchy by default, retain automatic message fallback for unanalyzed sessions and unsummarized tails, and expose an explicit URL-backed option to include every normalized conversation.

**Architecture:** Keep the existing `session_fts` index and select eligible rows at query time by joining each FTS document to its session and, for tail messages, its canonical message sequence. Apply one shared eligibility predicate to tokenized FTS and literal fallback queries. Propagate a strict `include_messages` boolean through the HTTP API, then render it as a non-persistent checkbox beside the embedded search field.

**Tech Stack:** Go 1.24, SQLite FTS5, standard `net/http`, embedded HTML/CSS/JavaScript, temporary real SQLite databases, Go `httptest`, and the existing Node VM browser-behavior harness.

## Global Constraints

- Default text search uses generated session and segment documents for sessions with stored successful analysis.
- Sessions without stored successful analysis remain searchable through all normalized message documents.
- Sessions with newer messages remain searchable through only message documents after `analyzed_through_sequence`.
- `IncludeMessages: true` adds every normalized message document for every session.
- Multi-word queries retain AND semantics within one eligible FTS document.
- Keyword search uses only FTS `title` and `body`; `working_directory` remains available only through the dedicated folder filter.
- Tokenized FTS and punctuation-only literal fallback use identical document eligibility.
- `GET /api/sessions` accepts `include_messages=true`, `include_messages=false`, or omission; any other supplied value returns `400 invalid_query`.
- The unchecked **Include full conversations** checkbox is URL-backed and is not persisted in SQLite, application settings, or `localStorage`.
- No migration, FTS rebuild, duplicate index, automatic analysis, or analyzer invocation is introduced.
- Existing filters, sorting, snippets, cursor pagination, refresh behavior, and selected-session preservation remain intact.

---

### Task 1: Store-Level Focused Search Eligibility

**Files:**
- Modify: `internal/store/search.go:16-197`
- Modify: `internal/store/search_test.go:11-249`

**Interfaces:**
- Consumes: existing `session_fts.document_type`, `session_fts.document_key`, `sessions.analysis_provider`, `sessions.analyzed_through_sequence`, and canonical `messages.sequence`.
- Produces: `store.SearchQuery.IncludeMessages bool`.
- Produces: `(*Store).textMatches(context.Context, string, bool)` and `(*Store).literalMatches(context.Context, string, bool)`.
- Invariant: both match paths use the same `searchDocumentEligibility` SQL predicate.

- [ ] **Step 1: Write failing focused-scope store tests**

Update transcript-only cases in `TestSearchTextAndCombinedFilters` so the
default is empty and the explicit scope restores them:

```go
{name: "analyzed transcript excluded", query: SearchQuery{Text: "database is locked"}, want: nil},
{name: "analyzed transcript included", query: SearchQuery{Text: "database is locked", IncludeMessages: true}, want: []string{"s2"}},
{name: "analyzed filename excluded", query: SearchQuery{Text: "internal/auth.go"}, want: nil},
{name: "analyzed filename included", query: SearchQuery{Text: "internal/auth.go", IncludeMessages: true}, want: []string{"s1"}},
{name: "literal analyzed transcript excluded", query: SearchQuery{Text: ":"}, want: nil},
{name: "literal analyzed transcript included", query: SearchQuery{Text: ":", IncludeMessages: true}, want: []string{"s2"}},
```

Keep generated hierarchy and unanalyzed fallback coverage:

```go
{name: "generated concept", query: SearchQuery{Text: "idempotency ledger"}, want: []string{"s1"}},
{name: "unicode generated title", query: SearchQuery{Text: "café retry"}, want: []string{"s3"}},
{name: "unanalyzed message fallback", query: SearchQuery{Text: "archive old ledger"}, want: []string{"s4"}},
```

Add a partial-session behavior test:

```go
func TestSearchFocusedScopeIncludesOnlyUnsummarizedTail(t *testing.T) {
	database := searchFixture(t)
	ctx := context.Background()
	detail, err := database.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	session := detail.Session
	session.SourceSize++
	session.SourceHash = "h1-appended"
	session.SourceMTime = session.SourceMTime.Add(time.Minute)
	session.LastActiveAt = session.LastActiveAt.Add(time.Minute)
	messages := append([]Message(nil), detail.Messages...)
	messages = append(messages, Message{
		Sequence: 2, Timestamp: session.LastActiveAt, Role: "assistant",
		Text: "Unsummarized zircon checksum result",
	})
	if _, err := database.ImportSession(ctx, session, messages); err != nil {
		t.Fatal(err)
	}

	assertSearchQueryIDs(t, database, SearchQuery{Text: "zircon checksum"}, []string{"s1"})
	assertSearchQueryIDs(t, database, SearchQuery{Text: "internal auth"}, nil)
	assertSearchQueryIDs(t, database, SearchQuery{Text: "internal auth", IncludeMessages: true}, []string{"s1"})
}
```

Add stored-provenance tests that distinguish first analysis from reanalysis:

```go
func TestSearchFocusedScopeUsesStoredAnalysisProvenance(t *testing.T) {
	for _, status := range []string{"queued", "running", "failed"} {
		t.Run(status, func(t *testing.T) {
			database := searchFixture(t)
			ctx := context.Background()
			if err := database.SetAnalysisStatus(ctx, "s2", status, "retry later"); err != nil {
				t.Fatal(err)
			}
			assertSearchQueryIDs(t, database, SearchQuery{Text: "database is locked"}, nil)

			if err := database.SetAnalysisStatus(ctx, "s4", status, "not analyzed"); err != nil {
				t.Fatal(err)
			}
			assertSearchQueryIDs(t, database, SearchQuery{Text: "archive old ledger"}, []string{"s4"})
		})
	}
}
```

Add folder-column separation:

```go
func TestSearchKeywordsExcludeWorkingDirectoryColumn(t *testing.T) {
	database := searchFixture(t)
	assertSearchQueryIDs(t, database, SearchQuery{Text: "work search"}, nil)
	assertSearchQueryIDs(t, database, SearchQuery{Text: "work search", IncludeMessages: true}, nil)
	assertSearchQueryIDs(t, database, SearchQuery{CWD: "work/search"}, []string{"s2"})
}
```

Replace `assertSearchIDs` with a query-aware helper:

```go
func assertSearchQueryIDs(t *testing.T, database *Store, query SearchQuery, want []string) {
	t.Helper()
	result, err := database.SearchSessions(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if got := hitIDs(result.Hits); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("SearchSessions(%#v) IDs = %v, want %v", query, got, want)
	}
}

func assertSearchIDs(t *testing.T, database *Store, text string, want []string) {
	t.Helper()
	assertSearchQueryIDs(t, database, SearchQuery{Text: text}, want)
}
```

- [ ] **Step 2: Run the focused store tests and confirm the red state**

Run:

```sh
go test ./internal/store -run 'TestSearch(TextAndCombinedFilters|FocusedScope|Keywords)' -count=1
```

Expected: compilation fails because `SearchQuery.IncludeMessages` does not
exist. After adding only that field to expose the intended test API, rerun and
confirm transcript-only analyzed-session cases fail because the current query
still searches all FTS rows.

- [ ] **Step 3: Implement one shared eligibility predicate**

Add the field:

```go
type SearchQuery struct {
	Text            string
	IncludeMessages bool
	// Existing fields remain unchanged.
}
```

Add one SQL predicate used by both query paths:

```go
const searchDocumentEligibility = `(
    session_fts.document_type IN ('session', 'segment')
    OR (
        session_fts.document_type = 'message'
        AND (
            ? = 1
            OR sessions.analysis_provider IS NULL
            OR messages.sequence > sessions.analyzed_through_sequence
        )
    )
)`
```

Pass the scope from `SearchSessions`:

```go
matches, err = s.textMatches(ctx, query.Text, query.IncludeMessages)
```

Change the tokenized path to column-scope FTS and join the canonical message
sequence:

```go
func (s *Store) textMatches(ctx context.Context, text string, includeMessages bool) (map[string]textMatch, error) {
	matchQuery := safeFTSQuery(text)
	if matchQuery == "" {
		return s.literalMatches(ctx, text, includeMessages)
	}
	matchQuery = "{title body} : (" + matchQuery + ")"
	rows, err := s.db.QueryContext(ctx, `
        SELECT session_fts.session_id,
               coalesce(nullif(snippet(session_fts, 4, '', '', '...', 18), ''), session_fts.title, ''),
               bm25(session_fts, 0.0, 0.0, 0.0, 10.0, 3.0, 0.0) AS score
        FROM session_fts
        JOIN sessions ON sessions.id = session_fts.session_id
        LEFT JOIN messages
          ON session_fts.document_type = 'message'
         AND messages.session_id = session_fts.session_id
         AND session_fts.document_key = printf('message:%d', messages.sequence)
        WHERE session_fts MATCH ?
          AND `+searchDocumentEligibility+`
        ORDER BY score`, matchQuery, includeMessages)
	if err != nil {
		return s.literalMatches(ctx, text, includeMessages)
	}
	// Existing row scan and best-row-per-session logic remains unchanged.
}
```

Apply the same joins and predicate to literal fallback while excluding the
working-directory column:

```go
func (s *Store) literalMatches(ctx context.Context, text string, includeMessages bool) (map[string]textMatch, error) {
	pattern := "%" + escapeLike(strings.TrimSpace(text)) + "%"
	rows, err := s.db.QueryContext(ctx, `
        SELECT session_fts.session_id,
               substr(coalesce(nullif(session_fts.body, ''), session_fts.title, ''), 1, 240)
        FROM session_fts
        JOIN sessions ON sessions.id = session_fts.session_id
        LEFT JOIN messages
          ON session_fts.document_type = 'message'
         AND messages.session_id = session_fts.session_id
         AND session_fts.document_key = printf('message:%d', messages.sequence)
        WHERE lower(session_fts.title || char(10) || session_fts.body)
              LIKE lower(?) ESCAPE '\'
          AND `+searchDocumentEligibility, pattern, includeMessages)
	// Existing row scan and best-row-per-session logic remains unchanged.
}
```

SQLite receives the Go boolean as the `? = 1` scope argument. The exact
`message:<sequence>` join prevents a malformed message key from qualifying as
an analyzed session's unsummarized tail.

- [ ] **Step 4: Run focused and complete store tests**

Run:

```sh
gofmt -w internal/store/search.go internal/store/search_test.go
go test ./internal/store -run 'TestSearch' -count=1
go test ./internal/store -count=1
```

Expected: PASS. Existing relevance, AND-query, filter, sort, cursor, delete, and
reanalysis tests remain green.

- [ ] **Step 5: Commit the store unit**

```sh
git add internal/store/search.go internal/store/search_test.go
git commit -m "feat: focus search on generated analysis"
```

---

### Task 2: Strict HTTP Search-Scope Contract

**Files:**
- Modify: `internal/httpapi/api.go:805-831`
- Modify: `internal/httpapi/api_test.go:249-293`

**Interfaces:**
- Consumes: `store.SearchQuery.IncludeMessages`.
- Produces: `GET /api/sessions?include_messages=true|false`.
- Invariant: omission and `false` are focused mode; no alternate boolean spellings are accepted.

- [ ] **Step 1: Write failing parser and endpoint tests**

Add a table test for the pure parser:

```go
func TestParseSearchQueryIncludeMessages(t *testing.T) {
	tests := []struct {
		name    string
		values  url.Values
		want    bool
		wantErr bool
	}{
		{name: "absent", values: url.Values{}},
		{name: "true", values: url.Values{"include_messages": {"true"}}, want: true},
		{name: "false", values: url.Values{"include_messages": {"false"}}},
		{name: "empty", values: url.Values{"include_messages": {""}}, wantErr: true},
		{name: "numeric", values: url.Values{"include_messages": {"1"}}, wantErr: true},
		{name: "mixed case", values: url.Values{"include_messages": {"TRUE"}}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query, err := parseSearchQuery(test.values)
			if test.wantErr {
				if err == nil {
					t.Fatal("parseSearchQuery succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if query.IncludeMessages != test.want {
				t.Fatalf("IncludeMessages = %v, want %v", query.IncludeMessages, test.want)
			}
		})
	}
}
```

Extend `TestSessionReadEndpoints` with the analyzed transcript-only phrase from
the existing fixture:

```go
response = serve(handler, apiRequest(http.MethodGet,
	"/test-token/api/sessions?q=sensitive+transcript+phrase", nil))
decodeResponse(t, response, &search)
if len(search.Sessions) != 0 {
	t.Fatalf("focused transcript search response = %#v", search)
}

response = serve(handler, apiRequest(http.MethodGet,
	"/test-token/api/sessions?q=sensitive+transcript+phrase&include_messages=true", nil))
decodeResponse(t, response, &search)
if len(search.Sessions) != 1 || search.Sessions[0].ID != "session-1" {
	t.Fatalf("full transcript search response = %#v", search)
}

response = serve(handler, apiRequest(http.MethodGet,
	"/test-token/api/sessions?include_messages=1", nil))
if response.Code != http.StatusBadRequest ||
	!strings.Contains(response.Body.String(), `"code":"invalid_query"`) {
	t.Fatalf("invalid include_messages response = %d %s", response.Code, response.Body.String())
}
```

- [ ] **Step 2: Run the HTTP tests and confirm the red state**

Run:

```sh
go test ./internal/httpapi -run 'Test(ParseSearchQueryIncludeMessages|SessionReadEndpoints)' -count=1
```

Expected: `TestParseSearchQueryIncludeMessages` fails because the parser ignores
the parameter, and the full-message endpoint returns no session because the
scope is not forwarded.

- [ ] **Step 3: Parse only the exact public values**

Add this block after constructing `store.SearchQuery`:

```go
if values.Has("include_messages") {
	switch values.Get("include_messages") {
	case "true":
		query.IncludeMessages = true
	case "false":
	default:
		return store.SearchQuery{}, errors.New("include_messages must be true or false")
	}
}
```

Do not use `strconv.ParseBool`, because it accepts values outside the documented
contract.

- [ ] **Step 4: Run focused and complete HTTP tests**

Run:

```sh
gofmt -w internal/httpapi/api.go internal/httpapi/api_test.go
go test ./internal/httpapi -run 'Test(ParseSearchQueryIncludeMessages|SessionReadEndpoints)' -count=1
go test ./internal/httpapi -count=1
```

Expected: PASS, including the embedded Node behavior harness.

- [ ] **Step 5: Commit the HTTP unit**

```sh
git add internal/httpapi/api.go internal/httpapi/api_test.go
git commit -m "feat: expose full conversation search scope"
```

---

### Task 3: URL-Backed Search-Scope Checkbox

**Files:**
- Modify: `internal/httpapi/assets/index.html:27-32`
- Modify: `internal/httpapi/assets/app.css:45-50,144-175`
- Modify: `internal/httpapi/assets/app.js:25-145,554-631`
- Modify: `internal/httpapi/testdata/app_behavior_test.js:9-150,350-357`
- Modify: `internal/httpapi/api_test.go:106-194`

**Interfaces:**
- Consumes: `include_messages=true` HTTP parameter from Task 2.
- Produces: `#include-messages-filter`, checked only from the current URL.
- Produces: `updateSearchPlaceholder()` and search requests that add the parameter only while checked.
- Invariant: refresh, pagination, and selection preserve scope through existing URL and in-memory controls; **Clear all** restores focused mode.

- [ ] **Step 1: Write failing embedded-markup and browser behavior tests**

Add `include-messages-filter` to the required HTML IDs in
`TestWebApplicationIncludesCoreWorkflows`, and require these exact strings:

```go
`Include full conversations`,
`placeholder="Search summaries and topics"`,
```

Require JavaScript behavior strings:

```go
`params.set("include_messages", "true")`,
`elements["include-messages-filter"].checked = params.get("include_messages") === "true";`,
`function updateSearchPlaceholder()`,
`Search summaries and conversations`,
```

Require CSS contracts:

```go
`.search-controls {`,
`.include-messages-control {`,
`flex-wrap: wrap;`,
```

Add the new ID to `elementIDs` in `app_behavior_test.js`. Extend
`createEnvironment` to accept URL state and record replacements:

```js
function createEnvironment(storedPreference, search = "") {
  // Existing setup remains.
  const historyPaths = [];
  const environment = {
    // Existing fields remain.
    historyPaths
  };
  const context = vm.createContext({
    // Existing globals remain.
    location: { pathname: "/test-token/", search },
    history: { replaceState(_state, _unused, path) { historyPaths.push(path); } },
  });
}
```

Add behavior coverage:

```js
async function testSearchScopeControl() {
  const focused = createEnvironment(undefined, "?q=ledger");
  await settle();
  assert.equal(focused.nodes["include-messages-filter"].checked, false);
  assert.equal(focused.nodes["session-search"].placeholder, "Search summaries and topics");
  assert.equal(evaluate(focused, 'searchParams().has("include_messages")'), false);

  focused.nodes["include-messages-filter"].checked = true;
  await dispatch(focused, "include-messages-filter", "change");
  assert.equal(focused.nodes["session-search"].placeholder, "Search summaries and conversations");
  assert.equal(evaluate(focused, 'searchParams().get("include_messages")'), "true");
  assert.match(focused.historyPaths.at(-1), /include_messages=true/);

  evaluate(focused, "clearFilters()");
  assert.equal(focused.nodes["include-messages-filter"].checked, false);
  assert.equal(focused.nodes["session-search"].placeholder, "Search summaries and topics");
  assert.equal(evaluate(focused, 'searchParams().has("include_messages")'), false);

  const restored = createEnvironment("off", "?q=ledger&include_messages=true");
  await settle();
  assert.equal(restored.nodes["include-messages-filter"].checked, true);
  assert.equal(restored.nodes["session-search"].placeholder, "Search summaries and conversations");
  assert.equal(evaluate(restored, 'searchParams("next").get("include_messages")'), "true");
  assert.equal(evaluate(restored, 'searchParams("next").get("cursor")'), "next");

  restored.requests.length = 0;
  await dispatch(restored, "refresh-button", "click");
  assert.ok(restored.requests.some((request) =>
    request.url.includes("/sessions?") && request.url.includes("include_messages=true")
  ));
}
```

Call `await testSearchScopeControl()` from `main()`.

- [ ] **Step 2: Run embedded web tests and confirm the red state**

Run:

```sh
go test ./internal/httpapi -run 'Test(WebApplicationIncludesCoreWorkflows|BrowserRefreshBehavior)' -count=1
```

Expected: FAIL because the checkbox markup, JavaScript state, and CSS contracts
do not exist.

- [ ] **Step 3: Add compact responsive markup and styling**

Replace the lone search label with:

```html
<div class="search-controls">
  <label class="search-field">
    <span class="sr-only">Search sessions</span>
    <input id="session-search" name="q" type="search" autocomplete="off" placeholder="Search summaries and topics">
  </label>
  <label class="include-messages-control">
    <input id="include-messages-filter" type="checkbox">
    <span>Include full conversations</span>
  </label>
</div>
```

Use stable, wrapping layout:

```css
.filter-bar { grid-template-columns: minmax(420px, 1fr) 125px 145px 145px auto; }
.search-controls { min-width: 0; display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.search-field { min-width: 180px; flex: 1; }
.search-field input { width: 100%; }
.include-messages-control { min-height: 34px; display: inline-flex; align-items: center; gap: 6px; color: var(--text-muted); font-size: 12px; font-weight: 650; white-space: nowrap; }
.include-messages-control input { min-height: auto; margin: 0; }
```

At the existing 800px breakpoint, make `.search-controls`, rather than
`.search-field`, span the filter grid:

```css
.search-controls { grid-column: 1 / -1; }
```

- [ ] **Step 4: Implement URL, request, placeholder, and clear behavior**

In `setURLFromFilters`:

```js
if (elements["include-messages-filter"].checked) params.set("include_messages", "true");
```

In `restoreFilters`:

```js
elements["include-messages-filter"].checked = params.get("include_messages") === "true";
updateSearchPlaceholder();
```

In `searchParams`:

```js
if (elements["include-messages-filter"].checked) params.set("include_messages", "true");
```

Add:

```js
function updateSearchPlaceholder() {
  elements["session-search"].placeholder = elements["include-messages-filter"].checked
    ? "Search summaries and conversations"
    : "Search summaries and topics";
}
```

In `clearFilters`, before `applyFilters()`:

```js
elements["include-messages-filter"].checked = false;
updateSearchPlaceholder();
```

Register the scope event separately from `filterIDs`:

```js
elements["include-messages-filter"].addEventListener("change", () => {
  updateSearchPlaceholder();
  applyFilters();
});
```

Do not add the checkbox to `filterIDs`: those controls use string `.value`
semantics and removable filter chips, while this checkbox has explicit boolean
and visible state.

- [ ] **Step 5: Run browser behavior, syntax, and HTTP package tests**

Run:

```sh
node --check internal/httpapi/assets/app.js
node internal/httpapi/testdata/app_behavior_test.js internal/httpapi/assets/app.js
go test ./internal/httpapi -count=1
```

Expected: JavaScript syntax succeeds, the harness prints
`browser behavior assertions passed`, and the Go package passes.

- [ ] **Step 6: Commit the browser unit**

```sh
git add internal/httpapi/assets/index.html internal/httpapi/assets/app.css \
  internal/httpapi/assets/app.js internal/httpapi/testdata/app_behavior_test.js \
  internal/httpapi/api_test.go
git commit -m "feat: add focused search scope control"
```

---

### Task 4: Documentation, Full Verification, And Local Runtime

**Files:**
- Modify: `README.md:137-167`
- Modify: `docs/DESIGN.md:528-556`
- Modify: `docs/IMPLEMENTATION_PLAN.md:244-281,317-350`
- Modify: `docs/superpowers/specs/2026-07-17-focused-search-scope-design.md:3-4`

**Interfaces:**
- Documents: focused default, automatic unanalyzed/tail fallback, explicit full-conversation scope, and exact API parameter.
- Verifies: the public binary and the existing loopback LaunchAgent deployment.

- [ ] **Step 1: Update public and architecture documentation**

Change the README search bullet to:

```markdown
- focused full-text search over generated titles, summaries, and topics, with
  automatic normalized-message fallback for unanalyzed sessions and new
  unsummarized activity;
- an **Include full conversations** option that also searches all normalized
  visible messages and retained tool facts;
```

State that working directories use the dedicated folder filter, and document
`include_messages=true` as the API opt-in for full normalized-message search.

Update `docs/DESIGN.md` and `docs/IMPLEMENTATION_PLAN.md` so they no longer
describe all message documents and working directories as default keyword
matches. Preserve the existing fact that all document types remain materialized
in FTS.

Change the feature spec status to:

```markdown
**Status:** Implemented
```

- [ ] **Step 2: Run the complete automated verification suite**

Run:

```sh
gofmt -w internal/store/search.go internal/store/search_test.go \
  internal/httpapi/api.go internal/httpapi/api_test.go
node --check internal/httpapi/assets/app.js
node internal/httpapi/testdata/app_behavior_test.js internal/httpapi/assets/app.js
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
go build ./cmd/agent-history
git diff --check
```

Expected: every command exits zero, the browser harness prints its pass line,
and no analyzer process is invoked.

- [ ] **Step 3: Restart the existing local service and verify health**

Use the repository's committed service scripts rather than introducing another
startup mechanism:

```sh
./scripts/install-macos-service.sh
curl --fail --silent --show-error http://127.0.0.1:54321/api/health
```

Expected health response:

```json
{"status":"ok"}
```

Open `http://127.0.0.1:54321/` in the in-app browser and verify at desktop and
approximately 390 CSS pixels:

- the checkbox is unchecked by default;
- checking it changes the placeholder and preserves current filters/selection;
- automatic and manual refresh preserve the checkbox;
- the search row does not overlap or create horizontal overflow; and
- focused and full-conversation searches return the expected different result
  sets for a known analyzed transcript-only phrase.

- [ ] **Step 4: Inspect the final diff and commit documentation**

```sh
git status --short
git diff --check
git diff --stat HEAD~3
git add README.md docs/DESIGN.md docs/IMPLEMENTATION_PLAN.md \
  docs/superpowers/specs/2026-07-17-focused-search-scope-design.md
git commit -m "docs: explain focused session search"
```

Expected: only the search-scope plan, store/API/UI implementation, tests, and
matching documentation are part of this feature.

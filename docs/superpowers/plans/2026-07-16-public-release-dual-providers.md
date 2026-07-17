# Public Release And Dual CLI Providers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish Agent History as an MIT-licensed public repository whose local Go service can analyze sessions through either an authenticated Codex CLI or an authenticated Claude Code CLI selected in the web UI.

**Architecture:** Keep `analyze.Analyzer` as the model boundary and add one fixed Claude adapter beside the existing Codex adapter. Discover both executables at startup, expose a read-only provider catalog through the existing settings API, persist one selected provider/model, and reload it at each manual or scheduled enqueue boundary. Publish only after identity migration, documentation, local/clean-clone/CI/browser verification, history rewriting, and secret scans pass.

**Tech Stack:** Go 1.24 module with Go 1.26.5 toolchain, `os/exec`, SQLite/FTS5, embedded HTML/CSS/JavaScript, Node's built-in test runtime, POSIX shell/macOS LaunchAgents, GitHub Actions, Dependabot, Gitleaks, and GitHub CLI.

## Global Constraints

- The public module path is exactly `github.com/jaintarun/agent-history`.
- The fixed service remains exactly `http://127.0.0.1:54321/`.
- Analyzer IDs are exactly `codex-cli` and `claude-cli`.
- Default models are `gpt-5.4-mini` for Codex and `haiku` for Claude.
- Selection never silently falls back to another provider.
- Claude must not use `--bare`; it reuses the installed CLI's authentication.
- Tests use fake executables and never consume provider usage.
- No API-key manager, arbitrary commands, remote provider, per-stage routing, model API discovery, Linux startup integration, binary release automation, or plugin system.
- Existing transcript privacy, atomic analysis, loopback HTTP, SQLite, and embedded-UI invariants remain unchanged.
- Git history is rewritten to `3188229+jaintarun@users.noreply.github.com` only while the repository is private.
- The license is MIT with copyright `2026 Tarun Jain`.

---

### Task 1: Add The Claude Adapter And Fixed Provider Catalog

**Files:**
- Create: `internal/analyze/claudecli.go`
- Create: `internal/analyze/claudecli_test.go`
- Create: `internal/analyze/providers.go`
- Create: `internal/analyze/providers_test.go`
- Modify: `internal/analyze/engine.go`
- Modify: `internal/analyze/engine_test.go`

**Interfaces:**
- Consumes: `Analyzer.Generate`, `StructuredRequest`, and existing bounded CLI helpers.
- Produces: `NewClaudeCLI`, `Provider`, provider constants, `DiscoverCLIProviders`, and `FindProvider`.

- [ ] **Step 1: Write the failing safe-invocation test**

```go
func TestClaudeCLIGenerateUsesSafeNonPersistentSchemaInvocation(t *testing.T) {
	executable, argsPath, stdinPath := fakeClaudeExecutable(t)
	t.Setenv("FAKE_CLAUDE_OUTPUT", `{"type":"result","structured_output":{"title":"ok"}}`)
	analyzer := NewClaudeCLI(executable)
	request := StructuredRequest{Kind: RequestLeaf, Prompt: "untrusted transcript", Schema: json.RawMessage(`{"type":"object"}`)}
	result, err := analyzer.Generate(context.Background(), "haiku", request)
	if err != nil { t.Fatal(err) }
	if string(result) != `{"title":"ok"}` { t.Fatalf("result = %s", result) }
	arguments, err := os.ReadFile(argsPath)
	if err != nil { t.Fatal(err) }
	for _, required := range []string{
		"--safe-mode", "-p", "--no-session-persistence", "--tools", "",
		"--disable-slash-commands", "--permission-mode", "dontAsk", "--max-turns", "1",
		"--output-format", "json", "--json-schema", string(request.Schema), "--model", "haiku",
	} {
		if !strings.Contains(string(arguments), required+"\n") { t.Errorf("missing %q:\n%s", required, arguments) }
	}
	if strings.Contains(string(arguments), "--bare\n") { t.Fatalf("--bare present:\n%s", arguments) }
	stdin, err := os.ReadFile(stdinPath)
	if err != nil { t.Fatal(err) }
	if string(stdin) != request.Prompt { t.Fatalf("stdin = %q", stdin) }
}
```

The fake executable writes one argv item per line, copies stdin, optionally
sleeps/writes stderr, prints `FAKE_CLAUDE_OUTPUT`, and exits with
`FAKE_CLAUDE_EXIT`.

- [ ] **Step 2: Add failing error-path cases**

Use this exact table plus dedicated timeout/nonzero/oversize tests:

```go
for _, test := range []struct{ name, output, want string }{
	{"invalid envelope", `{broken`, "invalid JSON"},
	{"missing output", `{"type":"result"}`, "structured_output is missing"},
	{"null output", `{"structured_output":null}`, "structured_output is missing"},
} {
	t.Run(test.name, func(t *testing.T) {
		executable, _, _ := fakeClaudeExecutable(t)
		t.Setenv("FAKE_CLAUDE_OUTPUT", test.output)
		_, err := NewClaudeCLI(executable).Generate(context.Background(), "haiku", testStructuredRequest())
		if err == nil || !strings.Contains(err.Error(), test.want) { t.Fatalf("error = %v", err) }
	})
}
```

Set `MaxOutputBytes=128` for oversized stdout and truncated stderr cases; set
`Timeout=30*time.Millisecond` with a one-second fake sleep; cancel a context
before invocation. Assert `errors.Is` for deadline/cancellation and a bounded
error containing `exit status 7` plus `[truncated]` for nonzero exit.

- [ ] **Step 3: Confirm the adapter tests fail**

Run: `go test ./internal/analyze -run '^TestClaudeCLI' -count=1`

Expected: compilation fails because `NewClaudeCLI` is undefined.

- [ ] **Step 4: Implement `ClaudeCLI` minimally**

Use the existing timeout/output defaults, a temporary cwd, prompt on stdin, and:

```go
args := []string{
	"--safe-mode", "-p", "--no-session-persistence", "--tools", "",
	"--disable-slash-commands", "--permission-mode", "dontAsk", "--max-turns", "1",
	"--output-format", "json", "--json-schema", string(request.Schema), "--model", model,
}
```

Capture bounded stdout/stderr. Reject truncated stdout. Decode only:

```go
var envelope struct {
	StructuredOutput json.RawMessage `json:"structured_output"`
}
if err := json.Unmarshal(stdout.buffer.Bytes(), &envelope); err != nil {
	return nil, fmt.Errorf("claude-cli: result is invalid JSON: %w", err)
}
if len(envelope.StructuredOutput) == 0 || string(envelope.StructuredOutput) == "null" {
	return nil, errors.New("claude-cli: structured_output is missing")
}
if !json.Valid(envelope.StructuredOutput) {
	return nil, errors.New("claude-cli: structured_output is invalid JSON")
}
return envelope.StructuredOutput, nil
```

Validate nonempty model/prompt and valid schema before process creation. Prefix
all errors with `claude-cli:`.

- [ ] **Step 5: Run and pass adapter tests**

```sh
gofmt -w internal/analyze/claudecli.go internal/analyze/claudecli_test.go
go test ./internal/analyze -run '^TestClaudeCLI' -count=1
```

Expected: PASS without a real provider call.

- [ ] **Step 6: Write failing four-state discovery tests**

```go
func TestDiscoverCLIProviders(t *testing.T) {
	for _, test := range []struct {
		name string
		installed map[string]string
		available []bool
		adapters int
	}{
		{"both", map[string]string{"codex":"/bin/codex", "claude":"/bin/claude"}, []bool{true,true}, 2},
		{"codex", map[string]string{"codex":"/bin/codex"}, []bool{true,false}, 1},
		{"claude", map[string]string{"claude":"/bin/claude"}, []bool{false,true}, 1},
		{"neither", map[string]string{}, []bool{false,false}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(name string) (string, error) {
				if path, ok := test.installed[name]; ok { return path, nil }
				return "", exec.ErrNotFound
			}
			providers, adapters := DiscoverCLIProviders(lookup)
			if len(providers) != 2 || len(adapters) != test.adapters { t.Fatalf("%#v %d", providers, len(adapters)) }
			if providers[0].ID != "codex-cli" || providers[0].DefaultModel != "gpt-5.4-mini" || providers[0].Available != test.available[0] { t.Fatalf("%#v", providers[0]) }
			if providers[1].ID != "claude-cli" || providers[1].DefaultModel != "haiku" || providers[1].Available != test.available[1] { t.Fatalf("%#v", providers[1]) }
		})
	}
}
```

- [ ] **Step 7: Implement the catalog and clearer engine failure**

```go
const (
	ProviderCodexCLI = "codex-cli"
	ProviderClaudeCLI = "claude-cli"
	DefaultCodexModel = "gpt-5.4-mini"
	DefaultClaudeModel = "haiku"
)

type Provider struct {
	ID, Name, DefaultModel string
	Available bool
}
```

`DiscoverCLIProviders(lookPath)` always returns Codex then Claude metadata and
constructs adapters only for found paths. `FindProvider` performs a linear
two-item lookup. Change both Engine unknown-provider errors to:

```go
fmt.Errorf("analysis provider %q is unavailable; install its CLI and restart Agent History", options.Provider)
```

Add an engine test proving this error preserves current analysis.

- [ ] **Step 8: Verify and commit**

```sh
gofmt -w internal/analyze/providers.go internal/analyze/providers_test.go internal/analyze/engine.go internal/analyze/engine_test.go
go test ./internal/analyze -count=1
git add internal/analyze
git commit -m "feat: add Claude CLI analysis provider"
```

---

### Task 2: Wire Discovery And Reload Scheduled Settings

**Files:**
- Modify: `cmd/agent-history/main.go`
- Modify: `cmd/agent-history/main_test.go`

**Interfaces:**
- Consumes: provider discovery, SQLite settings, Engine, Worker.
- Produces: detected new-database default, persisted provider loading, dynamic pending-batch option loading.

- [ ] **Step 1: Write failing preference and persisted-setting tests**

```go
func TestPreferredAnalysisUsesInstalledCLI(t *testing.T) {
	for _, test := range []struct{ name string; codex, claude bool; provider, model string }{
		{"both", true, true, "codex-cli", "gpt-5.4-mini"},
		{"codex", true, false, "codex-cli", "gpt-5.4-mini"},
		{"claude", false, true, "claude-cli", "haiku"},
		{"neither", false, false, "codex-cli", "gpt-5.4-mini"},
	} {
		t.Run(test.name, func(t *testing.T) {
			providers := []analyze.Provider{
				{ID:"codex-cli", DefaultModel:"gpt-5.4-mini", Available:test.codex},
				{ID:"claude-cli", DefaultModel:"haiku", Available:test.claude},
			}
			got := preferredAnalysis(providers)
			if got.Provider != test.provider || got.Model != test.model { t.Fatalf("%#v", got) }
		})
	}
}
```

With a temporary real store, set `analysis.provider=claude-cli`,
`analysis.model=sonnet`, and `analysis.auto=false`; assert `analysisSettings`
returns all three. Add config cases proving Claude without a model chooses
`haiku`, an explicit model wins, and provider `shell` is rejected.

- [ ] **Step 2: Write the failing live scheduler test**

```go
func TestEnqueuePendingAnalysesLoadsCurrentOptionsForEachBatch(t *testing.T) {
	database := &fakePendingStore{ids: []string{"session-1"}}
	queue := &fakeAnalysisQueue{}
	current := analyze.Options{Provider:"codex-cli", Model:"first", PromptVersion:"v1", NormalizerVersion:"v1"}
	load := func(context.Context) (analyze.Options, error) { return current, nil }
	if _, err := enqueuePendingAnalyses(context.Background(), database, queue, load, false); err != nil { t.Fatal(err) }
	current.Provider, current.Model = "claude-cli", "haiku"
	if _, err := enqueuePendingAnalyses(context.Background(), database, queue, load, false); err != nil { t.Fatal(err) }
	if len(queue.jobs) != 2 || queue.jobs[0].Provider != "codex-cli" || queue.jobs[1].Provider != "claude-cli" { t.Fatalf("%#v", queue.jobs) }
}
```

- [ ] **Step 3: Confirm focused tests fail**

Run: `go test ./cmd/agent-history -run 'TestPreferred|TestAnalysisSettings|TestEnqueuePendingAnalysesLoads' -count=1`

Expected: missing helper/provider load and old pending signature failures.

- [ ] **Step 4: Implement startup discovery and seeding**

Before opening SQLite:

```go
providers, analyzers := analyze.DiscoverCLIProviders(exec.LookPath)
configuredAnalysis := preferredAnalysis(providers)
```

`preferredAnalysis` selects available Codex, then available Claude, then Codex.
If TOML specifies a supported provider, select its default model before applying
an explicit TOML model. Reject unsupported providers and blank/over-200 models.
Construct Engine with `analyzers` and pass `providers` to HTTP config.

- [ ] **Step 5: Load provider from SQLite and options per pending batch**

Add the same `database.Setting` read for `analysis.provider` that already exists
for model. Change pending helper to:

```go
type analysisOptionsLoader func(context.Context) (analyze.Options, error)

func enqueuePendingAnalyses(ctx context.Context, database pendingAnalysisStore, queue analysisQueue,
	load analysisOptionsLoader, includeFailed bool) (int, error) {
	options, err := load(ctx)
	if err != nil { return 0, fmt.Errorf("load analysis settings: %w", err) }
	ids, err := database.PendingAnalysisSessionIDs(ctx, includeFailed)
	if err != nil { return 0, err }
	queued := 0
	var queueErrors []error
	for _, id := range ids {
		if _, err := queue.Enqueue(ctx, id, options); err != nil {
			if !errors.Is(err, analyze.ErrAlreadyQueued) {
				queueErrors = append(queueErrors, fmt.Errorf("queue session %s: %w", id, err))
			}
			continue
		}
		queued++
	}
	return queued, errors.Join(queueErrors...)
}
```

Production supplies a closure that calls `analysisSettings` each time the scan
callback queues a batch. Startup-recovered jobs keep startup options.

- [ ] **Step 6: Verify and commit**

```sh
gofmt -w cmd/agent-history/main.go cmd/agent-history/main_test.go
go test ./cmd/agent-history -count=1
go test ./... -count=1
git add cmd/agent-history/main.go cmd/agent-history/main_test.go
git commit -m "feat: select installed analysis providers"
```

---

### Task 3: Expose And Validate Providers In Settings API

**Files:**
- Modify: `internal/httpapi/api.go`
- Modify: `internal/httpapi/api_test.go`
- Modify: `internal/httpapi/server_test.go`

**Interfaces:**
- Consumes: `[]analyze.Provider` and saved settings.
- Produces: `Config.AnalysisProviders`, JSON catalog, two-provider validation.

- [ ] **Step 1: Write failing API tests**

Pass this catalog from `testHandler`:

```go
AnalysisProviders: []analyze.Provider{
	{ID:"codex-cli", Name:"Codex", DefaultModel:"gpt-5.4-mini", Available:true},
	{ID:"claude-cli", Name:"Claude Code", DefaultModel:"haiku", Available:false},
},
```

Assert GET settings returns both in stable order with `id`, `name`,
`default_model`, `available`, and no executable/auth/environment fields. PUT
`claude-cli`/`haiku`; assert Analyze and Retitle queue those saved values. Assert
400 for provider `shell`, whitespace model, and a 201-character model. Assert an
unavailable but supported selected provider survives a partial settings update.
Assert per-request Analyze accepts `claude-cli` and rejects `shell`.

- [ ] **Step 2: Confirm API tests fail**

Run: `go test ./internal/httpapi -run 'Test.*Settings|TestMutationEndpoints' -count=1`

Expected: missing catalog and single-provider rejection failures.

- [ ] **Step 3: Implement catalog response and validation**

Add `AnalysisProviders []analyze.Provider` to Config/handler and copy the slice
in `NewHandler`. Require the default provider to exist. Add:

```go
type analysisProviderResponse struct {
	ID string `json:"id"`
	Name string `json:"name"`
	DefaultModel string `json:"default_model"`
	Available bool `json:"available"`
}
```

Add `AnalysisProviders []analysisProviderResponse` to settings response and map
only public metadata. Use `analyze.FindProvider` for PUT and per-request Analyze
validation. Permit supported unavailable selections, reject unknown IDs, and
retain existing model limits. Do not expose resolved paths or auth state.

- [ ] **Step 4: Verify and commit**

```sh
gofmt -w internal/httpapi/api.go internal/httpapi/api_test.go internal/httpapi/server_test.go
go test ./internal/httpapi -count=1
go test ./... -count=1
git add internal/httpapi/api.go internal/httpapi/api_test.go internal/httpapi/server_test.go
git commit -m "feat: expose analyzer choices in settings"
```

---

### Task 4: Add The Provider Dropdown To The Embedded UI

**Files:**
- Modify: `internal/httpapi/assets/index.html`
- Modify: `internal/httpapi/assets/app.js`
- Modify: `internal/httpapi/assets/app.css`
- Modify: `internal/httpapi/testdata/app_behavior_test.js`
- Modify: `internal/httpapi/browser_behavior_test.go`

**Interfaces:**
- Consumes: settings catalog JSON.
- Produces: native provider select, unavailable state, conservative model defaulting.

- [ ] **Step 1: Write failing browser assertions**

Add two provider objects to the fake response, make `settings-provider` a
`SELECT`, and add `settings-provider-status` to fake IDs. Assert opening Settings
renders two options. Then:

```js
environment.nodes["settings-model"].value = "gpt-5.4-mini";
environment.nodes["settings-provider"].value = "claude-cli";
await dispatch(environment, "settings-provider", "change");
assert.equal(environment.nodes["settings-model"].value, "haiku");
environment.nodes["settings-model"].value = "custom-model";
environment.nodes["settings-provider"].value = "codex-cli";
await dispatch(environment, "settings-provider", "change");
assert.equal(environment.nodes["settings-model"].value, "custom-model");
```

In another environment mark Claude unavailable and assert its option is disabled
and labeled `Not installed`. Capture PUT and assert selected values are sent.

- [ ] **Step 2: Confirm browser test fails**

Run: `go test ./internal/httpapi -run '^TestBrowserBehavior$' -count=1`

Expected: options/defaulting assertions fail.

- [ ] **Step 3: Implement the select and status**

Replace readonly provider input with:

```html
<label>Provider<select id="settings-provider" required></select></label>
<p id="settings-provider-status" class="muted" role="status"></p>
```

Add `state.settingsProvider` and `state.settingsProviders`. Render catalog options
with name plus `(Not installed)`; disable unavailable options except an already
selected unavailable value. Status says detected auth will be reused, or asks
the user to install and restart. On change, replace model only when empty or
equal to the previous provider default. Set `dialog select` width to 100% and
bump script query from `v=3` to `v=4`.

- [ ] **Step 4: Verify and commit**

```sh
go test ./internal/httpapi -run 'TestBrowserBehavior|TestWebAssets' -count=1
go test ./... -count=1
git add internal/httpapi/assets internal/httpapi/testdata/app_behavior_test.js internal/httpapi/browser_behavior_test.go
git commit -m "feat: select analysis provider in web settings"
```

---

### Task 5: Correct Public Identity And Migrate Startup

**Files:**
- Modify: `go.mod`
- Modify: Go imports under `cmd/` and `internal/`
- Modify: `install-startup.sh`
- Modify: `uninstall-startup.sh`
- Modify: `run-local.sh`
- Modify: `internal/launch/startup_scripts_test.go`

**Interfaces:**
- Produces: correct module path, new LaunchAgent label, legacy cleanup, version `v0.2.0`.

- [ ] **Step 1: Write failing startup migration tests**

Change fake launchctl state to one file per service. Seed
`com.tarunjain.agent-history.plist` and its loaded state. After two installs
assert `io.github.jaintarun.agent-history.plist` exists/lints, its Label is the
new ID, the legacy plist/state are absent, and only one new job is loaded. After
two uninstalls assert both IDs/plists are absent. Copy the three scripts to a
temporary `Agent History Repo` directory and assert the plist retains the full
space-containing `run-local.sh` path.

- [ ] **Step 2: Confirm startup test fails**

Run: `go test ./internal/launch -run '^TestStartupScriptsAreIdempotent$' -count=1`

Expected: old label remains and migration assertions fail.

- [ ] **Step 3: Implement dual-label migration/removal**

Set `label="io.github.jaintarun.agent-history"` and
`legacy_label="com.tarunjain.agent-history"`. In install, bootout/wait/disable
and delete the legacy plist before the new-label idempotence check. In uninstall,
apply bootout/wait/disable/plist removal to both labels. Keep port ownership,
loopback health, logs, data preservation, and repeated-run behavior unchanged.

- [ ] **Step 4: Correct module/version identifiers**

Set:

```go
module github.com/jaintarun/agent-history
```

Replace only Go import prefixes from `github.com/tarunjain/agent-history` to the
new path. Change run-local linker version to `v0.2.0`; run `go mod tidy`.

- [ ] **Step 5: Verify and commit**

```sh
gofmt -w $(rg --files -g '*.go')
sh -n install-startup.sh uninstall-startup.sh run-local.sh
go test ./internal/launch -run '^TestStartupScriptsAreIdempotent$' -count=1
go test ./... -count=1
go list -m
git diff --check
git add go.mod go.sum cmd internal install-startup.sh uninstall-startup.sh run-local.sh
git commit -m "chore: adopt public repository identity"
```

Expected: module output is `github.com/jaintarun/agent-history`; legacy label
occurs only in migration code/tests/specs.

---

### Task 6: Add Public Docs, License, CI, And Dependabot

**Files:**
- Create: `LICENSE`
- Create: `SECURITY.md`
- Create: `CONTRIBUTING.md`
- Create: `CODE_OF_CONDUCT.md`
- Create: `.github/workflows/ci.yml`
- Create: `.github/dependabot.yml`
- Modify: `README.md`
- Modify: `docs/DESIGN.md`
- Modify: `docs/IMPLEMENTATION_PLAN.md`

**Interfaces:**
- Produces: concise public setup, accurate provider policy, community health files, CI.

- [ ] **Step 1: Record failing documentation checks**

```sh
for file in LICENSE SECURITY.md CONTRIBUTING.md CODE_OF_CONDUCT.md .github/workflows/ci.yml .github/dependabot.yml; do test -s "$file"; done
rg -n 'claude-cli|haiku|claude -p|127\.0\.0\.1:54321|MIT' README.md
```

Expected: missing-file and incomplete README failures.

- [ ] **Step 2: Add public policy files**

Add the standard MIT text with `Copyright (c) 2026 Tarun Jain`. SECURITY supports
latest `main` and directs private reports to
`https://github.com/jaintarun/agent-history/security/advisories/new`, forbidding
credentials/transcripts in public issues. CONTRIBUTING lists Go commands from
AGENTS.md, fake-provider requirement, privacy invariants, focused PRs, and banned
generated/database/transcript artifacts. CODE_OF_CONDUCT is Contributor Covenant
2.1 with standard attribution and enforcement contact
`https://github.com/jaintarun` (no personal email).

- [ ] **Step 3: Put this exact five-step quick start first**

1. Install/authenticate at least one analyzer: `npm install -g @openai/codex` plus
   `codex login`, and/or `brew install --cask claude-code` plus
   `claude auth login`.
2. Install cmux: `brew tap manaflow-ai/cmux`, `brew install --cask cmux`,
   `open -a cmux`, `cmux hooks setup`, `cmux hooks setup codex`.
3. Configure cmux: the two existing `defaults write` commands, restart cmux
   normally, and verify `cmux capabilities --json` reports `allowAll`.
4. Clone `https://github.com/jaintarun/agent-history.git`, enter it, run
   `./install-startup.sh`.
5. Open `http://127.0.0.1:54321/`, open Settings, choose Codex or Claude/model.

State cmux is optional for search/analysis and required only for live status,
title sync, color, and cmux resume.

- [ ] **Step 4: Document providers and current vendor policy**

Add a matrix: Codex/`codex exec`/`gpt-5.4-mini`; Claude Code/`claude -p`/`haiku`.
Link official OpenAI sign-in, Anthropic installation/authentication/CLI/model,
Anthropic Agent SDK subscription-policy, and cmux pages. State executable
detection requires service restart, credentials are never read/stored, exported
`ANTHROPIC_API_KEY` takes precedence over subscription OAuth in `-p`, Agent
History never implements Claude login/routes OAuth, Anthropic's separate-credit
change was paused and current policy says `-p` draws subscription usage, model
actions consume usage, scans/search do not, and no fallback occurs.

Retain concise run/port/data/analysis/search/cmux/privacy/backup/uninstall/
troubleshooting/development sections. Remove personal corpus measurements and
the hand-written plist example.

- [ ] **Step 5: Update canonical design/phase docs**

Show both CLI adapters in DESIGN architecture and provider examples. Rename
Phase 4 to CLI Analysis, add Claude fake-executable verification and either-CLI
exit condition, and remove Claude analyzer from deferred work. Do not change
other deferred items.

- [ ] **Step 6: Add pinned CI and weekly dependency checks**

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
permissions:
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7
        with:
          go-version-file: go.mod
          cache: true
      - run: go test ./...
      - run: go test -race ./...
      - run: go vet ./...
      - run: go build ./cmd/agent-history
```

Dependabot version 2 checks `gomod` and `github-actions` weekly at `/`, with
open-pull-request limit 5 for each.

- [ ] **Step 7: Verify and commit**

```sh
for file in LICENSE SECURITY.md CONTRIBUTING.md CODE_OF_CONDUCT.md .github/workflows/ci.yml .github/dependabot.yml; do test -s "$file"; done
rg -n 'claude-cli|haiku|claude -p|127\.0\.0\.1:54321|MIT License' README.md LICENSE
go test ./... -count=1
go vet ./...
go build ./cmd/agent-history
git diff --check
git add LICENSE SECURITY.md CONTRIBUTING.md CODE_OF_CONDUCT.md .github README.md docs/DESIGN.md docs/IMPLEMENTATION_PLAN.md
git commit -m "docs: prepare MIT public release"
```

---

### Task 7: Verify, Rewrite, Deploy, Publish, And Validate

**Files:**
- Modify: Git metadata and GitHub repository settings only.
- Verify: tree, clean clone, LaunchAgent, UI, public repository.

**Interfaces:**
- Produces: rewritten public `main`, available security features, healthy migrated service.

- [ ] **Step 1: Run the complete local gate**

```sh
gofmt -w $(rg --files -g '*.go')
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go build ./cmd/agent-history
sh -n install-startup.sh uninstall-startup.sh run-local.sh
git diff --check
git status --short --branch
```

Expected: exit 0 and clean tree; remove the generated root binary if present.

- [ ] **Step 2: Run history/tree secret scans**

```sh
go run github.com/zricethezav/gitleaks/v8@latest git --no-banner --redact=100 --report-format=json --report-path=/tmp/agent-history-gitleaks-git.json --log-opts='--all' .
go run github.com/zricethezav/gitleaks/v8@latest dir --no-banner --redact=100 --report-format=json --report-path=/tmp/agent-history-gitleaks-dir.json .
find . -type f \( -name '*.db' -o -name '*.db-wal' -o -name '*.db-shm' -o -name '*.pem' -o -name '*.key' -o -name '.env' -o -name '*.log' \) -not -path './.git/*' -print
```

Expected: zero leaks and no sensitive artifact paths.

- [ ] **Step 3: Test a clean clone**

```sh
clean_root=$(mktemp -d)
git clone --no-local . "$clean_root/agent-history"
(cd "$clean_root/agent-history" && go test ./... && go build ./cmd/agent-history && sh -n install-startup.sh uninstall-startup.sh run-local.sh)
rm -rf "$clean_root"
```

Expected: PASS without local config or provider calls.

- [ ] **Step 4: Migrate live startup and verify health/catalog**

```sh
./install-startup.sh
launchctl print "gui/$(id -u)/io.github.jaintarun.agent-history" >/dev/null
if launchctl print "gui/$(id -u)/com.tarunjain.agent-history" >/dev/null 2>&1; then exit 1; fi
curl -fsS http://127.0.0.1:54321/api/health
curl -fsS http://127.0.0.1:54321/api/settings
```

Expected: new service loaded, legacy absent, health OK, both providers listed
without paths/credentials.

- [ ] **Step 5: Verify UI at 1440x900 and 390x844**

Using the in-app browser, capture both screenshots. Verify list/filter/selection/
refresh state, Settings provider select, model default/preservation, save without
selection loss, no overlap, and no console/network errors. Do not submit Analyze.

- [ ] **Step 6: Rewrite old email while private**

```sh
test "$(gh repo view jaintarun/agent-history --json visibility --jq .visibility)" = PRIVATE
git fetch origin main
old_remote=$(git rev-parse origin/main)
before_tree=$(git rev-parse 'main^{tree}')
git branch backup/pre-public-main main
FILTER_BRANCH_SQUELCH_WARNING=1 git filter-branch -f --env-filter '
old_email="tjain73@gmail.com"
new_email="3188229+jaintarun@users.noreply.github.com"
if [ "$GIT_AUTHOR_EMAIL" = "$old_email" ]; then GIT_AUTHOR_EMAIL="$new_email"; fi
if [ "$GIT_COMMITTER_EMAIL" = "$old_email" ]; then GIT_COMMITTER_EMAIL="$new_email"; fi
export GIT_AUTHOR_EMAIL GIT_COMMITTER_EMAIL
' -- main
test "$before_tree" = "$(git rev-parse 'main^{tree}')"
test "$(git log main --format='%ae%n%ce' | sort -u)" = "3188229+jaintarun@users.noreply.github.com"
```

Expected: tree unchanged; all main author/committer emails are noreply.

- [ ] **Step 7: Force-push privately and require CI success**

```sh
git push --force-with-lease="main:$old_remote" origin main
run_id=$(gh run list --repo jaintarun/agent-history --workflow CI --branch main --limit 1 --json databaseId --jq '.[0].databaseId')
test -n "$run_id"
gh run watch "$run_id" --repo jaintarun/agent-history --exit-status
test "$(git rev-parse main)" = "$(git ls-remote origin refs/heads/main | cut -f1)"
```

- [ ] **Step 8: Publish and enable supported security controls**

```sh
gh repo edit jaintarun/agent-history --visibility public --accept-visibility-change-consequences
gh api -X PUT repos/jaintarun/agent-history/vulnerability-alerts
gh api -X PUT repos/jaintarun/agent-history/automated-security-fixes
gh api -X PUT repos/jaintarun/agent-history/private-vulnerability-reporting
gh api -X PATCH repos/jaintarun/agent-history --input - <<'JSON'
{"security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"}}}
JSON
gh api -X PATCH repos/jaintarun/agent-history/code-scanning/default-setup --input - <<'JSON'
{"state":"configured","query_suite":"default","languages":["go"]}
JSON
```

If GitHub returns 403/422 for a feature unavailable to this personal public
repository, record the exact response and continue only for that feature.
Visibility, vulnerability alerts, and private reporting are required.

- [ ] **Step 9: Verify public access and MIT detection unauthenticated**

```sh
curl -fsS https://api.github.com/repos/jaintarun/agent-history | jq -e '.private == false and .license.spdx_id == "MIT" and .default_branch == "main"'
curl -fsS https://raw.githubusercontent.com/jaintarun/agent-history/main/README.md | rg '127\.0\.0\.1:54321|claude-cli|codex-cli'
curl -fsS https://raw.githubusercontent.com/jaintarun/agent-history/main/LICENSE | rg 'MIT License|Copyright \(c\) 2026 Tarun Jain'
gh api repos/jaintarun/agent-history/private-vulnerability-reporting
```

- [ ] **Step 10: Remove local backup refs after public verification**

```sh
git branch -D backup/pre-public-main
git for-each-ref --format='%(refname)' refs/original/ | while IFS= read -r ref; do test -z "$ref" || git update-ref -d "$ref"; done
git status --short --branch
git log main --format='%ae%n%ce' | sort -u
```

Expected: clean synchronized branch and only the approved noreply email.

- [ ] **Step 11: Capture completion evidence**

```sh
git rev-parse HEAD
git log -1 --oneline
gh repo view jaintarun/agent-history --json url,visibility,defaultBranchRef
gh run list --repo jaintarun/agent-history --workflow CI --branch main --limit 1 --json url,status,conclusion,headSha
launchctl print "gui/$(id -u)/io.github.jaintarun.agent-history" | sed -n '1,40p'
curl -fsS http://127.0.0.1:54321/api/health
```

Expected: public rewritten SHA, successful CI, new running LaunchAgent, healthy
local service.

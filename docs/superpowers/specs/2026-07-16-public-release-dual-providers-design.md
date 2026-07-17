# Public Release And Dual CLI Providers Design

**Date:** 2026-07-16
**Status:** Approved for implementation planning

## Purpose

Prepare Agent History for an MIT-licensed public release and let users analyze
sessions with either an authenticated Codex CLI or an authenticated Claude Code
CLI. The release must remain a local, self-contained Go application backed by
SQLite, preserve the existing privacy invariants, and be usable from a concise
public quick start.

The public repository will be `github.com/jaintarun/agent-history`. The default
local service remains `http://127.0.0.1:54321/`.

## Release Scope

This release adds one production analyzer adapter, provider discovery and
selection, public project documentation, continuous integration, dependency
update configuration, and repository publication. It also corrects personal
repository identifiers that would make a public installation misleading.

This release does not add API-key management, arbitrary command templates,
remote analysis services, per-stage model routing, model discovery from vendor
APIs, binary release automation, Linux startup integration, or a generic plugin
system. A provider is a fixed application-owned adapter, not user-supplied
shell code.

## Analyzer Architecture

The existing `analyze.Analyzer` interface remains the sole model boundary.
Production registers two adapters by stable provider ID:

- `codex-cli`, implemented by the existing `CodexCLI` adapter;
- `claude-cli`, implemented by a new `ClaudeCLI` adapter.

Both adapters receive the same schema-constrained `StructuredRequest` and
return only the validated JSON payload expected by the analysis engine. Source
agent and analysis provider remain independent: a Claude transcript can be
summarized by Codex, and a Codex transcript can be summarized by Claude.

### Codex Invocation

Codex continues to run as an ephemeral `codex exec` request in a temporary
directory with read-only sandboxing, user rules and project instructions
ignored, bounded stdout/stderr, a bounded result file, and the prompt supplied
on standard input. Existing ChatGPT or API-key CLI authentication is reused;
Agent History never reads or stores credentials.

The default Codex model for a new database is `gpt-5.4-mini`.

### Claude Invocation

Claude runs in print mode through the installed `claude` executable. The
adapter invokes one non-persistent, schema-constrained request in an isolated
temporary directory with:

- `--safe-mode` to retain authentication while disabling user and project
  customizations;
- `-p`, `--output-format json`, and `--json-schema` for non-interactive
  structured output;
- `--no-session-persistence`, `--tools ""`, `--disable-slash-commands`,
  `--permission-mode dontAsk`, and `--max-turns 1` to remove useful tools and
  interactive behavior; and
- `--model <model>` for the selected model.

The prompt is supplied on standard input. The adapter extracts
`structured_output` from Claude's JSON envelope, rejects missing or invalid
structured output, enforces the same timeout and output limits as Codex, and
returns no surrounding CLI metadata to the engine.

The adapter does not use `--bare`, because bare mode bypasses OAuth/keychain
authentication and therefore does not support the intended subscription-based
workflow. The default Claude model for a new selection is the efficient
`haiku` alias. Users may enter another supported alias or full model name.

As of this design date, Anthropic's current published policy says the announced
separate Agent SDK credit was paused and `claude -p` continues to draw from the
user's Claude subscription usage limits. The README will link to the current
Anthropic policy instead of treating this behavior as permanent.

## Provider Discovery And Selection

At server startup, Agent History resolves `codex` and `claude` with
`exec.LookPath`. Discovery does not invoke either model and does not test
authentication. The server registers an adapter only when its executable is
found.

The settings API returns a fixed provider catalog containing both provider IDs,
human-readable names, availability, and default model. It does not return
executable paths or environment values. The Settings dialog uses this catalog
to render a provider dropdown. Installed providers are selectable; unavailable
providers remain visible and disabled so users can see how to enable them.

Provider and model are persisted in existing settings storage. The model field
remains editable. When the user switches provider, the UI proposes that
provider's default model only when the current model is empty or still equals
the previous provider's default; a user-entered model is never silently
overwritten. The server validates that the provider is one of the two supported
IDs and that both provider and model are nonempty and within existing length
limits.

For a new database, a valid provider/model explicitly supplied by the existing
local TOML configuration seeds the database first. Without that override, the
initial provider is selected in this order:

1. Codex when `codex` is installed;
2. Claude when Codex is absent and `claude` is installed;
3. Codex as the documented default when neither is installed.

Existing database settings are preserved. A selected provider that is not
currently installed remains selected, but analysis requests fail with an
actionable unavailable-provider message. Ingestion, search, browsing, deletion,
cmux reconciliation, and launch behavior continue normally.

Manual Analyze, Reanalyze, and Retitle requests load the current persisted
provider and model. The background pending-analysis queue must do the same for
each scan callback rather than retaining startup options. A setting change
therefore applies to subsequently queued work without a server restart. Already
running work is not canceled or moved to another provider, and the application
never silently falls back to a different provider.

## Web And API Behavior

The existing Settings dialog gains the provider dropdown and short installation
or authentication guidance when a provider is unavailable. No separate
provider-management page is added. The session detail continues to expose the
provider and model that produced its current summary.

The settings response and mutation remain on the existing API surface. Provider
catalog data is read-only; only the selected provider, model, automatic analysis
setting, and existing cmux title-sync setting are mutable. Existing Host,
Origin, loopback, and URL-token protections remain unchanged.

Provider errors shown in the UI distinguish these cases without exposing raw
environment details:

- executable not installed or not on the service `PATH`;
- CLI command timed out;
- CLI exited unsuccessfully, with bounded sanitized stderr;
- structured output missing, malformed, oversized, or schema-invalid; and
- authentication or usage-limit errors reported by the CLI.

The existing atomic analysis behavior is unchanged: a failed request preserves
the last successful visible analysis.

## Public Repository Hardening

The Go module and all internal imports change from
`github.com/tarunjain/agent-history` to
`github.com/jaintarun/agent-history`, matching the GitHub remote.

The project adds:

- an MIT `LICENSE` with copyright `2026 Tarun Jain`;
- a concise root `README.md` with a five-step quick start;
- `SECURITY.md` with private vulnerability-reporting instructions and supported
  version policy;
- `CONTRIBUTING.md` with setup, tests, privacy invariants, and pull-request
  expectations;
- a Contributor Covenant `CODE_OF_CONDUCT.md`;
- a GitHub Actions workflow that builds and tests the Go application with
  minimal permissions; and
- Dependabot configuration for Go modules and GitHub Actions.

The README's first screen gives these exact outcomes in five short steps:

1. install and authenticate Codex and/or Claude Code;
2. install cmux and its Codex/Claude hooks when cmux integration is wanted;
3. set cmux socket access to `allowAll`, disable cmux automatic naming, restart
   cmux, and verify capabilities;
4. clone the repository and run the idempotent macOS startup installer; and
5. open `http://127.0.0.1:54321/` and choose the analyzer in Settings.

Detailed sections explain foreground operation, changing the bind port, data
locations, provider usage implications, source discovery, cmux behavior,
privacy, backup, uninstall, development, and troubleshooting. Personal corpus
measurements and the redundant hand-written LaunchAgent example are removed
from the root README. The existing design documents remain the source for
algorithm and acceptance detail.

The LaunchAgent label changes from `com.tarunjain.agent-history` to
`io.github.jaintarun.agent-history`. Installation first removes the old loaded
job and old plist when present, then idempotently installs the new job. The
uninstaller removes both identifiers so upgrades and repeated removal leave no
duplicate startup process. Data and logs are preserved.

The fixed startup script continues to bind only to `127.0.0.1:54321`, uses
tokenless loopback mode, scans at startup and every 15 minutes, and processes
pending analysis. Direct `serve` flags remain the supported way to choose a
different port; no new configuration layer is added.

## Privacy And Publication

The full current tree and all Git history are scanned with Gitleaks before
publication. The audit already found no credentials, secret-like tracked files,
databases, logs, certificates, or keys. Scans are repeated after all release
changes.

Before publication, all 39 existing commits are rewritten from the personal
author/committer email to
`3188229+jaintarun@users.noreply.github.com`, retaining names, timestamps,
messages, and file contents. New commits use the same noreply address. Because
rewriting changes commit IDs, the rewritten `main` branch is force-pushed only
while the repository is still private and after a local backup reference is
created. The backup reference is removed locally after the public branch is
verified; it is never pushed.

After verification, GitHub repository visibility changes from private to
public. Available GitHub security features are enabled: vulnerability alerts,
automated security updates, secret scanning and push protection where the
account permits them, private vulnerability reporting, and CodeQL default setup
when supported for this Go repository. Repository visibility and the public
README are verified without relying on authenticated access.

## Testing Strategy

Development follows red-green-refactor behavior tests. Automated tests never
invoke a real model or consume subscription/API usage.

- `ClaudeCLI` tests use a fake executable to assert exact safety flags, stdin,
  working directory isolation, schema transport, envelope decoding, timeouts,
  nonzero exits, invalid JSON, missing `structured_output`, and output limits.
- Provider-discovery tests use temporary fake executables and controlled `PATH`
  values to cover Codex-only, Claude-only, both, and neither installed.
- Settings API tests cover the catalog, persisted selection, unavailable
  providers, invalid provider/model input, and preservation of user-entered
  models.
- Browser behavior tests cover dropdown rendering, disabled unavailable
  options, default-model proposal, saving, and retained selections.
- Scheduler tests prove that a provider/model change is read for later queued
  work without restart and does not alter work already running.
- Startup-script tests cover old-label migration, new-label idempotence, dual
  removal, fixed loopback port, and paths containing spaces.
- Existing analyzer, ingestion, store, HTTP, cmux, and UI tests remain green.

Release verification includes `gofmt`, `go test ./...`, `go test -race ./...`,
`go vet ./...`, `govulncheck ./...`, `go build ./cmd/agent-history`, shell syntax
checks, Gitleaks history and directory scans, a clean-clone build/test, and
desktop plus narrow browser checks of the running service. The clean clone must
work without local untracked configuration and must not invoke either provider
during tests.

## Completion Criteria

The release is complete only when:

- Codex and Claude are both visible provider choices and installed providers
  can complete schema-constrained analysis through fake-executable tests;
- changing the saved provider/model affects subsequent manual and scheduled
  analysis without restart and without implicit fallback;
- the public documentation accurately describes installation, authentication,
  usage consumption, cmux setup, port `54321`, privacy, and removal;
- the module path and LaunchAgent identifiers match the public repository;
- all verification and secret scans pass from both the working tree and a clean
  clone;
- the rewritten Git history contains the GitHub noreply email and no personal
  author/committer email;
- the GitHub repository is public under the MIT license with the intended
  security settings; and
- the existing local LaunchAgent is migrated and the live service is healthy at
  `http://127.0.0.1:54321/`.

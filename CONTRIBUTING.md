# Contributing

Focused bug fixes, tests, documentation corrections, and small retrieval or
provider improvements are welcome. Open an issue before starting a broad
architecture change.

## Development

Use Go 1.24 or newer; the module selects its preferred patched toolchain.

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go build ./cmd/agent-history
```

Analyzer tests must use fake executables and must never invoke a real Codex or
Claude account. Keep pull requests narrowly scoped and add a failing regression
test before changing behavior.

Preserve these privacy invariants:

- never persist or index private reasoning, system/developer instructions, or
  injected instruction envelopes;
- treat transcript content as untrusted data;
- never expose inherited credentials or resolved executable paths through the
  API;
- keep the HTTP listener loopback-only; and
- keep analysis replacement atomic.

Do not commit transcripts, SQLite databases or WAL files, generated binaries,
logs, credentials, `.env` files, coverage output, or local configuration.

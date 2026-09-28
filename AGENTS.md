# AGENTS.md — mcp-kit

Instructions for AI coding agents and human contributors. `CLAUDE.md` and
`GEMINI.md` are symlinks to this file. User-facing documentation is in
[README.md](README.md).

## Project

mcp-kit (`github.com/nv3to/mcp-kit`, package `mcpkit`) is a Go library for
building stdio MCP servers on the official SDK
(`github.com/modelcontextprotocol/go-sdk`).

| Path | What |
|---|---|
| `mcpkit.go` | `New`, `Tool`, `AddTool`, `Server`, the call middleware (gate + error rewriting) |
| `errors.go` | `Kind`, the five kinds, `Error`, `Errorf`, `classify` |
| `serve.go` | `Serve`: the stdio transport, the stdout redirect, the hold on EOF |
| `mcptest/` | the in-memory test client |
| `budget/` | `Cut`, the log `Store` with `Page` and `Search`, the fluff-removal steps |
| `egress/` | the forward proxy that reads from allowlisted hosts only; standard library only |
| `mcpkit_test.go` | the toy server that every behaviour test drives |
| `imports_test.go` | the dependency guard |
| `confine/` | `Profile`, `Command`, `Confined`, `Verify`, `Verified`: a command held to a profile, through `sandbox-exec` on macOS and bubblewrap on Linux; the escape suite and the probes behind `docs/confinement-macos.md` and `docs/confinement-linux.md` (label `sandbox`) |
| `confine/nettest/` | the network cases of the escape suite: a confined command against a real `egress` proxy (label `sandbox`) |
| `plugins/`, `third_party/go/` | Please plugin and pinned Go modules and toolchain |

## Commands

```sh
plz build //...                 # build everything
plz test //...                  # all tests, including lint
plz test //... --include lint   # lint alone: gofmt, go vet and shellcheck
plz test //:mcpkit_test         # the one test target
go test -run TestGate ./...     # one test, without plz
```

Before you report a change as done, run `plz test //...`. It includes gofmt
(`:lint_gofmt` in each package) and `go vet` (`//:vet`), from the plugin
`github.com/nv3to/lint-rules`, pinned in `plugins/BUILD`.
Shell scripts (`.sh`) are checked by shellcheck from the host PATH (`:lint_shellcheck`);
there are none yet. A new package ends its BUILD file with `lint()`
and is listed in `repo_lint(packages = [...])` in the root BUILD, or `//:vet`
fails.

To change a dependency version, edit `go.mod` **and** `third_party/go/BUILD`
to the same version (they must match exactly), then run
`plz hash --update //third_party/go/...`. Never type a hash by hand. Every
`go_repo` declares its licence, and `.plzconfig` accepts only permissive ones.

## Hard rules

Tests enforce these rules. Do not weaken a test to make a change pass.

- **Imports.** The only imports allowed are the standard library, the SDK's
  `mcp` package and this module. `TestImportsOnlyTheSDK` (`imports_test.go`)
  parses every file of `mcpkit`, `mcptest`, `budget`, `confine` and `egress`
  and fails on anything else; `egress` and `confine` may not import the SDK
  either, nor each other: a test that needs both goes in `confine/nettest`.
  When run by hand it covers the test files too.
- **No new dependencies.** Every server built on the library inherits them.
  The SDK's `auth` package is off limits, because it pulls in `golang-jwt`.
- **Stdio only.** Do not add an HTTP or SSE transport.
- **Nothing but protocol frames reaches stdout.** Log through
  `Server.Logger()` (stderr).
- **Registration never panics.** `AddTool` returns an error for every refusal,
  including a schema the SDK cannot derive (the panic is recovered).
- **The error shape is a contract.** It is `<kind>: <message>` in a single
  text block. The kinds `invalid`, `not_found`, `conflict`, `refused` and
  `internal` are a closed set. Adding or renaming one breaks clients.

## Making a change

- **New behaviour.** Add a case to the toy server in `mcpkit_test.go` and drive
  it through `mcptest`, so the test covers the real protocol path: schema
  derivation, the middleware and the error shape. Do not test through
  unexported helpers when the protocol can show the behaviour.
- **`Serve` tests** swap the process-wide `os.Stdin` and `os.Stdout`. Never
  mark them `t.Parallel()`.
- **Exported identifiers** each need a doc comment that says what the
  identifier does and why. pkg.go.dev is the reference documentation.
- **Public API changes** must update the API table and the behaviour section
  of `README.md` in the same change.
- **Commit messages** use the imperative mood and name the behaviour, not the
  file: `AddTool refuses a name longer than 128 bytes`.

## Ask a maintainer first

Open an issue before you:

- change the public API (exported identifiers, the error shape, the kinds);
- add a dependency;
- add a transport;
- add MCP features beyond tools, such as resources, prompts or completions.

The library is small on purpose.

# mcp-kit

An opinionated Go library for building [Model Context Protocol](https://modelcontextprotocol.io)
servers that speak over stdio, built on the official
[Go SDK](https://github.com/modelcontextprotocol/go-sdk).

You write a tool as a typed Go function. mcp-kit derives its JSON schemas, gives
every failure the same shape, keeps stdout clean for protocol frames, and lets
one hook veto any call. `mcptest` drives your server through the real protocol
in unit tests, with no process and no pipe.

[![Go Reference](https://pkg.go.dev/badge/github.com/nv3to/mcp-kit.svg)](https://pkg.go.dev/github.com/nv3to/mcp-kit)

## Install

```sh
go get github.com/nv3to/mcp-kit
```

Requires Go 1.27. The only dependency is `github.com/modelcontextprotocol/go-sdk`.
The import path ends in `mcp-kit`; the package name is `mcpkit`.

## Quick start

```go
package main

import (
	"context"
	"os"
	"strings"

	"github.com/nv3to/mcp-kit"
)

type echoIn struct {
	Text  string `json:"text" jsonschema:"the text to repeat"`
	Times int    `json:"times,omitempty" jsonschema:"how often to repeat it, default once"`
}

type echoOut struct {
	Text string `json:"text"`
}

func main() {
	s := mcpkit.New("echo", "0.1.0")

	err := mcpkit.AddTool(s, mcpkit.Tool{Name: "echo", Description: "Repeat the text."},
		func(ctx context.Context, in echoIn) (echoOut, error) {
			if in.Times < 0 {
				return echoOut{}, mcpkit.Errorf(mcpkit.Invalid, "times must not be negative")
			}
			return echoOut{Text: strings.Repeat(in.Text, max(in.Times, 1))}, nil
		})
	if err != nil {
		s.Logger().Error("register", "err", err)
		os.Exit(1)
	}

	if err := s.Serve(context.Background()); err != nil {
		s.Logger().Error("serve", "err", err)
		os.Exit(1)
	}
}
```

Point any MCP client at the built binary as a stdio server. The `echo` tool
advertises an input schema with a required `text` and an optional `times`. It
returns structured content, `{"text":"abab"}`, along with the same JSON as text.

## How it behaves

**Typed tools.** `AddTool` is a generic function, not a method, so the server
is its first argument. The input schema comes from `In`, the output schema from
`Out`. `json` tags name the properties, `omitempty` makes a property optional,
and a `jsonschema:"…"` tag becomes its description.

**Registration fails loudly.** `AddTool` returns an error instead of panicking
when:

- the tool has no description;
- the name is not 1–128 characters of `[A-Za-z0-9_.-]`;
- the name is already registered;
- a type has no usable schema.

Check that error. If you drop it, the server silently serves without the tool.

**One error shape.** A handler error becomes a tool result with `isError` set
and one text block, `<kind>: <message>`. The set of kinds is closed, so a client
can branch on the prefix:

| Kind | Constant | Meaning |
|---|---|---|
| `invalid` | `mcpkit.Invalid` | the arguments are well-formed but not acceptable |
| `not_found` | `mcpkit.NotFound` | the thing the call names does not exist |
| `conflict` | `mcpkit.Conflict` | the call is fine but the current state does not allow it |
| `refused` | `mcpkit.Refused` | the call is not allowed here (a closed gate, a policy) |
| `internal` | `mcpkit.Internal` | anything the server did not classify |

Return `mcpkit.Errorf(kind, format, args...)` to choose the kind. It can be
wrapped with `%w`, and the wrapping context is kept:
`conflict: while working: boom`. Any other error is reported as `internal`. The
SDK's argument-validation failures are reported as `invalid`. Protocol errors,
such as a call to an unknown tool, are left to the SDK.

**Stdout belongs to the protocol.** While `Serve` runs, `os.Stdout` points at
stderr. A stray `fmt.Println` in a handler therefore cannot corrupt a frame.
Logs go to stderr through `Server.Logger()`, a `*slog.Logger` that the SDK uses
too.

**No lost replies at EOF.** When stdin closes, `Serve` waits until every
request it has already read is answered, then returns `nil`. Without this, a
request piped in just before EOF would get no reply. When the context ends,
`Serve` returns the context's error.

## Gating tools

`SetGate` installs a hook that is consulted before every tool call. A non-nil
error refuses the call: the handler never runs, and the client gets a
`refused` result, or your own kind if you return an `*mcpkit.Error`.

```go
s.SetGate(func(tool string) error {
	if tool == "deploy" && !deploysEnabled() {
		return errors.New("deploys are switched off")
	}
	return nil
})
```

The gate is safe to change while the server is serving. Pass `nil` to remove
it.

## Testing a server

```go
func TestEcho(t *testing.T) {
	s := newServer() // your *mcpkit.Server, tools registered
	res := mcptest.Connect(t, s).Call("echo", map[string]any{"text": "ab", "times": 2})
	if res.IsError {
		t.Fatalf("tool error: %s", mcptest.Text(res))
	}
	var out echoOut
	mcptest.Decode(t, res, &out)
	if out.Text != "abab" {
		t.Errorf("got %q", out.Text)
	}
}
```

`Connect` links an in-memory client to the server and closes both when the test
ends. A session has three methods:

- `ListTools()` returns the tools as a client sees them.
- `Call(tool, args)` fails the test on a protocol error.
- `CallErr(tool, args)` returns the protocol error instead, for tests that
  expect one.

Two package functions read a result: `mcptest.Text(res)` and
`mcptest.Decode(t, res, &out)`.

## Bounding output

A tool that runs a command keeps the full output in a `budget.Store` and
returns a part that fits a cap, with the handle to reach the rest:

```go
store, err := budget.NewStore(dir, 20) // the 20 newest logs
log, err := store.Create()
cmd.Stdout, cmd.Stderr = log, log
cmd.Run()

text, next, end := log.Page(1, 4000)
text = budget.Clean(text, budget.StripColour, budget.ReplacePath(checkout, "."), budget.CollapseRepeats)
// return text, log.Handle(), next and end to the caller
```

- **A cap counts characters**, not bytes, and no character is ever split.
- **A position is a 1-based line number** of the log as written. Cleaning
  works on text already taken out, so it never moves a position.
- **`Cut`** cuts a text after the last whole line that fits and reports how
  many characters it left out.
- **`Page`** returns whole lines. Following `next` until `end` visits every
  line once. A line longer than the cap alone is returned cut to the cap, and
  `next` still moves on. `Page` has no error result; after a loop that ended
  early, `Err` says why.
- **`Search`** takes a pattern of the standard `regexp` package and returns at
  most `max` matches along with the true total. A pattern that does not compile
  is `invalid`.
- **`Open`** knows only the handles `Create` issued. Anything else, an empty
  handle or a path included, is `not_found`: a handle never becomes part of a
  file name. A log the store evicted or closed is `not_found` too.
- **`CollapseRepeats`** keeps the first copy of a block of lines that occurs
  again, directly after itself or further down, and writes the count under it:
  `[the 3 lines above occur 4 times]`.

## API

| Package | Identifier | Purpose |
|---|---|---|
| `mcpkit` | `New(name, version) *Server` | a server with no tools |
| | `AddTool[In, Out](s, Tool, handler) error` | register a typed tool |
| | `Tool{Name, Description}` | the tool descriptor; both fields required |
| | `(*Server).Serve(ctx) error` | serve on stdin/stdout |
| | `(*Server).SetGate(func(tool string) error)` | veto hook for every call |
| | `(*Server).Logger() *slog.Logger` | the stderr logger |
| | `(*Server).Unwrap() *mcp.Server` | the underlying SDK server |
| | `Kind`, `Invalid`, `NotFound`, `Conflict`, `Refused`, `Internal` | error kinds |
| | `Error{Kind, Message}`, `Errorf(kind, format, args...) error` | classified failures |
| `mcptest` | `Connect(t, s) *Session` | in-memory client session |
| | `(*Session).ListTools`, `Call`, `CallErr` | drive the server |
| | `Text(res)`, `Decode(t, res, &out)` | read a result |
| `budget` | `Cut(text, cap) (kept, omitted)` | the whole lines that fit the cap, and how much was left out |
| | `NewStore(dir, max) (*Store, error)` | keep at most `max` full logs in `dir` |
| | `(*Store).Create() (*Log, error)` | a new log, evicting the oldest when full |
| | `(*Store).Open(handle) (*Log, error)` | find a log again; `not_found` otherwise |
| | `(*Store).Close() error` | remove every log |
| | `(*Log).Write`, `Handle`, `Lines`, `Err` | the `io.Writer` for a command, its handle, its line count, the failure of a `Page` |
| | `(*Log).Page(from, cap) (text, next, end)` | whole lines from a position, within the cap |
| | `(*Log).Search(pattern, max) (matches, total, err)` | matching lines as `Match{Line, Text}`, and the true total |
| | `Step`, `Clean(text, steps...)` | fluff removal, step by step |
| | `StripColour`, `CollapseRepeats`, `ReplacePath(prefix, with)` | the steps |

Full documentation is on [pkg.go.dev](https://pkg.go.dev/github.com/nv3to/mcp-kit).

## Limits

- **Stdio only and no auth.** There is no HTTP or SSE transport, and the SDK's
  `auth` package is not used.
- **Tools only.** There are no resources, prompts, completions or tool
  annotations. `Unwrap()` is the escape hatch. A tool registered through it
  skips mcp-kit's registration checks but still passes through the gate and the
  error rewriting.
- **`Serve` changes process-wide state.** It reassigns the `os.Stdout`
  *variable*, not file descriptor 1. Code that captured `os.Stdout` earlier, or
  a child process that inherits fd 1, still writes to the real stdout. Run one
  `Serve` per process.
- **Wrap with `%w`.** The kind prefix is spliced out of the message by text
  match, so wrap an `Errorf` error with `fmt.Errorf("…: %w", err)`. An
  unrecognised `Kind` is reported as `internal`.
- **The gate also sees unknown tool names.** A refusing gate turns what would
  have been a protocol error into a `refused` result.
- **Batches are not tracked at EOF.** The no-lost-replies guarantee covers
  single requests, not JSON-RPC batches.
- **The logger is fixed.** It writes to stderr at slog's default level (Info),
  and there is no option to replace it.

## Development

The build uses [Please](https://please.build) 17.32 with a pinned Go toolchain:

```sh
plz build //...
plz test //...
```

Plain `go build ./...` and `go test ./...` work as well. See [AGENTS.md](AGENTS.md)
for the rules a change must follow. It is written for AI coding agents and
humans alike.

## Status

The API is pre-1.0 (`v0.x`), and a minor release may change it.

## License

[MIT](LICENSE)

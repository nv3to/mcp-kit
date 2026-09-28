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

## Egress proxy

A confined command often has to download its dependencies. Package `egress`
is a forward proxy for that: it reads from the hosts you name and refuses
everything else. It uses the standard library only and knows nothing of
`confine`. On its own it is a proxy that nothing is forced to use;
[`confine.Proxy`](#a-command-that-reaches-the-proxy-and-nothing-else) makes it
the one destination a command can reach.

```go
p, err := egress.New(egress.Config{
	Allow: []string{"proxy.golang.org", "*.githubusercontent.com"},
	OnRefuse: func(r egress.Refusal) {
		log.Printf("egress refused %s %s: %s", r.Method, r.Host, r.Reason)
	},
})
if err != nil {
	return err
}
addr, err := p.Start(ctx) // 127.0.0.1 and a port the system chose
if err != nil {
	return err
}
defer p.Close()
```

| Request | Served when | Otherwise refused as |
|---|---|---|
| any | the host matches `Allow`: an exact name, a `*.suffix` pattern (subdomains only), or a listed literal address | `host not allowed` |
| any | the host is not, and does not resolve to, a loopback, private, unique-local, link-local, unspecified, multicast, reserved or metadata address, IPv4 or IPv6 | `address not allowed` |
| plain HTTP | the method is `GET` or `HEAD` | `method not allowed` |
| plain HTTP, `CONNECT` | there is no body and no content length | `body not allowed` |
| plain HTTP | the port is 80 | `port not allowed` |
| `CONNECT` | the port is 443 | `port not allowed` |

A refused request gets `403` and one plain line, `egress: <reason>`, and
`OnRefuse` receives the host, the method and the reason. The reasons are a
closed set.

- **The address is checked where it counts.** The proxy resolves a name once,
  refuses if any answer is forbidden, and dials the address it checked, never
  the name. A name that resolves differently a moment later changes nothing.
  The default dialer checks the address again as the socket connects. Listing
  a forbidden address in `Allow` does not make it reachable.
- **Redirects are the client's.** The proxy hands a redirect back as it is.
  The client follows it, and the next hop is checked as a request of its own.
- **Tunnels are opaque.** `CONNECT` carries TLS end to end; the proxy does not
  intercept it, so inside a tunnel it cannot tell a read from a write.
- **Limits.** The header size, the idle time of a tunnel and the number of
  client connections are bounded: `MaxHeaderBytes`, `IdleTimeout`, `MaxConns`.
- **A narrow channel remains.** An allowed host that accepts uploads over
  `GET` parameters, or over anything inside a tunnel, can still receive data.
  Allow only hosts you would trust with what the command can read.

## Confining a command

Package `confine` starts a command that can touch only what a profile names.
You state once what the command may read, write and see of the environment;
everything else is out of reach, and the network is closed unless you narrow
it to a proxy.

```go
profile := confine.Profile{
	ReadOnly:  []string{"/opt/homebrew"},
	ReadWrite: []string{checkout},
	Env: confine.Env{
		Pass: []string{"PATH", "LANG"},
		Set:  map[string]string{"CI": "1"},
	},
}
cmd, err := confine.Command(ctx, profile, "make", "test")
if err != nil {
	return err // refused: nothing was started
}
cmd.Dir = checkout
cmd.Stdout, cmd.Stderr = log, log
err = cmd.Run()
```

**`confine` does not confine the server itself.** The server runs as the
user, with the user's files. Only the commands it starts through `Command`
are held to a profile.

- **It fails closed.** `Command` returns `refused`, and starts nothing, when
  the sandbox of the system is missing or cannot start, when it cannot
  express the profile, or when the server is itself confined: a sandbox
  cannot start inside a sandbox. There is no mode that runs the command
  unconfined. The sandbox of macOS is `/usr/bin/sandbox-exec`. On Linux it is
  bubblewrap, `bwrap` on the `PATH`, which needs unprivileged user
  namespaces: while the system refuses them, `Command` refuses and names the
  setting, and [docs/confinement-linux.md](docs/confinement-linux.md) says
  how a human allows them. On every other system `Command` refuses.
- **Paths are resolved first.** Each path is made absolute and followed
  through its symlinks, because the sandbox matches the path a file really
  has. A path that does not exist is `not_found`, not a rule dropped in
  silence. A symlink inside a writable path that points out of it does not
  work for the command.
- **Everything is denied that the profile does not allow.** On macOS the
  sandbox starts from `(deny default)`. A command can read and execute the
  paths of its profile and what a program needs to run at all: `/System`,
  `/bin`, `/sbin`, `/usr/bin`, `/usr/sbin`, `/usr/lib`, `/usr/libexec`,
  `/usr/share`, `/private/var/db/dyld` and `/private/var/select`. It can read
  the time zone, the root directory and the device files `null`, `zero`,
  `random` and `urandom`. It cannot read `/etc`, `/Library`, `/Applications`,
  `/usr/local`, `/opt` or a home directory, and it cannot execute a file that
  lies anywhere else. No service of the system is in reach: `/usr/bin/open`,
  Apple events and `launchctl` fail. Name the directory of a tool that lives
  elsewhere, such as `/opt/homebrew`, in `ReadOnly`.
- **On Linux the root is empty.** bubblewrap binds into it, read-only, what
  a program needs to run: `/usr/bin`, `/usr/sbin`, `/usr/lib`, `/usr/lib32`,
  `/usr/lib64`, `/usr/libx32`, `/usr/libexec` and `/usr/share`; `/bin`,
  `/sbin`, `/lib`, `/lib32`, `/lib64` and `/libx32`, as the directories or
  the links the host has; and `/etc/ld.so.cache`, `/etc/localtime` and
  `/etc/alternatives`. Each is there where the host has it. The command gets
  a `/dev` of its own with the device files `null`, `zero`, `full`, `random`,
  `urandom` and `tty`, and a `/proc` of its own. Nothing else of `/etc` is
  there, and neither is `/home`, `/root`, `/opt`, `/usr/local`, `/var`,
  `/run` or `/tmp` beside the command's own temporary directory. Its user,
  process, IPC, host name, control group and network namespaces are its own,
  so the loopback of the host, its network and its abstract unix sockets are
  out of reach. Name the directory of a tool that lives elsewhere, such as
  `/usr/local`, in `ReadOnly`. A program whose path holds `=` is `refused`.
- **On Linux a unix socket in the profile is in reach.** A command can
  connect to a unix socket file that lies inside a path of its profile,
  read-only or writable, and so reach the service that listens there. On
  macOS the same connect is denied. Leave the directories of such sockets,
  as of an SSH agent or a container engine, out of the profile.
- **The longer path wins.** A read-only path inside a writable one stays
  read-only, and the directories that lead to it cannot be moved or removed.
  A writable path inside a read-only one can be written. A path that is in
  both lists is read-only. A writable path inside the system is `refused`.
- **The kernel answers for the machine only.** A command can ask for the
  processor, the memory and the version of the system. It cannot list the
  processes of the host: on Linux its `/proc` lists its own processes alone.
- **Accounts do not resolve.** A command that looks up a user by name or
  number gets no answer, so `id` prints numbers.
- **The environment is exact.** The command gets the names in `Env.Pass`
  that the server has set, the pairs in `Env.Set`, `TMPDIR` and
  `MCPKIT_CONFINED=1`, the proxy variables when the network is a proxy, and
  nothing else.
- **A temporary directory of its own.** Each command gets a fresh directory,
  named by `TMPDIR`, that no other confined command can see. `Wait` removes
  it, also when the command was killed through the context.
- **The working directory** must lie inside a readable path. Left empty, it
  is the temporary directory.
- **`Confined`** is true under a sandbox that `confine` started, which sets
  `MCPKIT_CONFINED`, and under any other that refuses a sandbox inside it. On
  Linux that is any user namespace but the host's, such as bubblewrap's or a
  rootless container's. A container that runs as root shares the user
  namespace of the host, so there `Confined` is false, and `Command` refuses
  only when bubblewrap cannot start.
- **`Verify`** runs commands under a profile, watches them fail to read and
  write outside it, and records the verdict in a directory you choose.
  `Verified` reads it back, after a restart too, and is false once the system
  changed. Keep that directory out of every profile's writable paths:
  `Command` refuses a profile that makes it writable once the directory was
  given to `Verify` or `Verified`.

### A command that reaches the proxy and nothing else

`Profile.Network` is `confine.None` unless you set it. `confine.Proxy(addr)`
takes the address an `egress` proxy listens on and makes it the only
destination of the command, so the allowlist of the proxy is the network
policy. Neither package imports the other: the address is all they share.

```go
p, err := egress.New(egress.Config{Allow: []string{"proxy.golang.org"}})
if err != nil {
	return err
}
addr, err := p.Start(ctx)
if err != nil {
	return err
}
defer p.Close()

profile := confine.Profile{
	ReadWrite: []string{checkout},
	Env:       confine.Env{Pass: []string{"PATH"}},
	Network:   confine.Proxy(addr),
}
cmd, err := confine.Command(ctx, profile, "go", "mod", "download")
```

- **One destination.** The command can connect to `addr`. Every other
  connection is refused, to a public address and to another loopback port
  alike, and the command cannot listen. With `None` the proxy is out of reach
  as well.
- **Names do not resolve inside.** The command hands the name to the proxy,
  which resolves it outside the sandbox and checks the address.
- **Certificates are read from a file.** With a proxy, and only then, the
  command can read `/private/etc/ssl`, where macOS keeps the certificates it
  trusts. `curl` finds them there. A program written in Go asks the trust
  daemon of macOS, which is out of reach because it fetches what a
  certificate names from outside the sandbox: set `SSL_CERT_FILE` to
  `/private/etc/ssl/cert.pem` in `Env.Set` and it verifies against the file.
- **The proxy variables are set for you.** The command finds
  `http://<addr>` in `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy` and
  `https_proxy`, and an empty `NO_PROXY` and `no_proxy`, so that no host is
  exempt. `Env` may not name these six under any network: `Command` returns
  `invalid`. A command that ignores them reaches nothing.
- **The address is a port of a loopback address**, written as
  `127.0.0.1:3128`, which is what `(*Proxy).Start` returns. Anything else,
  a name included, is `invalid`.
- **On Linux the proxy is out of reach.** The network namespace of the
  command holds a loopback of its own and nothing else, so it cannot reach
  the loopback of the host where the proxy listens: under `Proxy`, as under
  `None`, the command reaches no address of the network. A unix socket inside
  a path of the profile is no address of the network and stays in reach.

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
| `egress` | `New(Config) (*Proxy, error)` | a proxy for an allowlist; fails on a malformed entry |
| | `Config{Allow, OnRefuse, Resolver, Dial, MaxHeaderBytes, IdleTimeout, MaxConns}` | the allowlist, the refusal callback, the replaceable resolver and dialer, the limits |
| | `(*Proxy).Start(ctx) (addr string, err error)` | serve on a loopback port the system chooses, until `ctx` ends |
| | `(*Proxy).Serve(net.Listener) error` | serve on your own listener, such as a unix socket |
| | `(*Proxy).Close() error` | stop listening and close every connection and tunnel |
| | `Refusal{Host, Method, Reason}` | one refused request |
| | `Reason`, `HostNotAllowed`, `MethodNotAllowed`, `BodyNotAllowed`, `AddressNotAllowed`, `PortNotAllowed` | why it was refused |
| | `Resolver` | the lookup `*net.Resolver` implements |
| | `DefaultMaxHeaderBytes`, `DefaultIdleTimeout`, `DefaultMaxConns` | the limits a zero `Config` gets |
| `confine` | `Profile{ReadOnly, ReadWrite, Env, Network}` | what a command may touch |
| | `Env{Pass, Set}` | the names copied from the server's environment, and fixed pairs |
| | `Network`, `None` | what the command may reach; `None` closes the network |
| | `Proxy(addr string) Network` | the network narrowed to the proxy that listens on `addr` |
| | `Command(ctx, profile, name, args...) (*Cmd, error)` | the command held to the profile; `refused` when that cannot be guaranteed |
| | `Cmd{Stdout, Stderr, Dir}`, `(*Cmd).Run`, `Start`, `Wait` | used like an `exec.Cmd`; `Wait` removes the temporary directory |
| | `Confined() bool` | whether this process is under a sandbox already |
| | `Verify(ctx, dir) error` | probe the sandbox and record the verdict in `dir` |
| | `Verified(dir) (ok bool, reason string)` | read the verdict recorded in `dir` |
| | `Marker`, `TempVar` | the names of the variables every confined command gets |

Full documentation is on [pkg.go.dev](https://pkg.go.dev/github.com/nv3to/mcp-kit).

## Limits

- **Stdio only and no auth.** There is no HTTP or SSE transport for MCP, and
  the SDK's `auth` package is not used.
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

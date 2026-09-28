# Confinement on macOS: what Seatbelt can express

Five probes in `confine/probe_darwin_test.go` start a trivial command through
`/usr/bin/sandbox-exec` under a small profile and assert what happens.

- `E:` is an observation. Each one is an assertion of the named test, so it
  holds on any host where `plz test //confine/...` passes.
- `H:` is an inference that no test asserts.
- Observed on: Darwin 27.0.0 (`kern.osrelease`, which each test also logs).

The probes start sandboxes, so they fail inside one (see probe 3). A confined
runner leaves them out with `plz test //... --exclude sandbox`.

Every profile below starts with:

```scheme
(version 1)
(allow default)
```

## 1. Outbound traffic to one loopback port

Test: `TestLoopbackPort`. Two listeners on `127.0.0.1`, ports chosen by the
kernel; the profile names the port of the first.

```scheme
(deny network*)
(allow network-outbound (remote ip "localhost:<port>"))
```

- E: a TCP connection to `127.0.0.1:<port>` succeeds.
- E: a TCP connection to the other loopback port fails with `EPERM`.
- E: a TCP connection to the public address `1.1.1.1:443` fails with `EPERM`.
- E: a later rule overrides an earlier one: the `allow` after `deny network*`
  takes effect.
- H: the refusal is made at `connect(2)` and no packet leaves the host.
- H: `::1` is covered by `localhost` as well; the probe dials IPv4 only.

**For u7 to quote:** a Seatbelt profile can allow outbound traffic to one loopback port
and nothing else, with `(deny network*)` followed by
`(allow network-outbound (remote ip "localhost:<port>"))`.

Consequence for u7: the proxy listens on a loopback TCP port and the profile
names that port. The fallback of a unix socket plus a forwarder is not needed.
Consequence for u5: the profile is generated per run, because the port is only
known once the listener exists.

## 2. Name resolution with the network narrowed

Test: `TestNameResolution`. Same profile as probe 1. The confined process
resolves `example.com` with a deadline of 5 seconds; the same lookup outside
the sandbox is the control and must succeed.

- E: the lookup fails. It returns an error before the deadline; it neither
  succeeds nor hangs.
- H: the resolver reaches mDNSResponder over the unix socket
  `/private/var/run/mDNSResponder`, which `(deny network*)` covers.
- H: a name in `/etc/hosts`, `localhost` included, may still resolve. It is
  not probed.

Consequence for u7: a confined client cannot resolve names. It is given the
proxy as the literal `127.0.0.1:<port>`, and the proxy resolves names outside
the sandbox.
Consequence for u5: the profile needs no rule for the resolver.

## 3. A profile applied inside a confined process

Test: `TestNesting`. `sandbox-exec` runs under the base profile and starts a
second `sandbox-exec` with the same profile.

- E: the inner `sandbox-exec` exits with status 71 and does not run the
  command.
- E: its standard error contains `sandbox_apply: Operation not permitted`.
- E: this holds when the outer profile is `(allow default)`, so no rule of the
  outer profile causes it.

**For plz-mcp u7 to quote:** a sandbox cannot be started inside a sandbox on macOS:
under any profile, `sandbox-exec` exits with status 71 and prints
`sandbox_apply: Operation not permitted`. Nesting is refused, not merged.

Consequence for u5: confinement is applied once, by the outermost process that
is not yet confined. Code that may already run confined detects it (probe 5)
and does not try again.
Consequence for u7: the proxy runs outside the sandbox, started before the
confined child.

## 4. File reads under the home directory

Test: `TestHomeReads`. The fixture is a directory created under the home
directory of the account, with an `allowed` subdirectory, a file in it, a file
next to it, and a symlink in `allowed` that points to that second file.
Commands run with `/` as working directory.

```scheme
(deny file-read* (subpath "<home>"))
(allow file-read* (subpath "<home>/<fixture>/allowed"))
```

- E: `/bin/cat` reads the file inside `allowed`, although every parent
  directory is denied.
- E: `/bin/cat` of the file outside `allowed` fails with
  `Operation not permitted`.
- E: `/bin/cat` of the symlink inside `allowed` that points outside fails with
  `Operation not permitted`: the rule is matched against the target, not
  against the path that was opened.
- H: paths in a profile must be resolved first (`/var` is `/private/var`),
  because rules match resolved paths. The probe resolves the home directory
  before writing the profile.
- H: a program whose binary, working directory or cache is under the home
  directory needs each of them listed. A real build tool is not probed.

Consequence for u5: the profile denies the home directory and lists subpaths,
in that order, with every path resolved. A symlink cannot be used to leave an
allowed directory, and one that points out of it stops working.
Consequence for u7: none. The file rules and the network rules of probe 1 are
independent and go in the same profile.

## 5. Whether a process can tell it is confined

Test: `TestDetection`. The test binary runs as a child, outside a sandbox and
under the base profile.

- E: the environment of the child is the same in both cases. `sandbox-exec`
  sets no marker.
- E: a child that is not confined starts `sandbox-exec` with the base profile
  and `/usr/bin/true`, and it exits with status 0.
- E: a confined child that does the same gets status 71 and
  `sandbox_apply: Operation not permitted`.
- H: `sandbox_check(3)` from `libsystem_sandbox` answers the same question
  without starting a process. It needs cgo, which the probes do not use.

Consequence for u5: a process detects confinement by trying to start a sandbox
with `(allow default)`. A caller that wants a cheaper signal sets its own
environment variable when it starts the confined child.
Consequence for u7: none beyond probe 3.

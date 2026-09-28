# Confinement on macOS: what Seatbelt can express

Five probes in `confine/probe_darwin_test.go` start a trivial command through
`/usr/bin/sandbox-exec` under a small profile and assert what happens.

- `E:` is an observation. Each one is an assertion of the named test, so it
  holds on any host where `plz test //confine/...` passes.
- `H:` is an inference that no test asserts.
- Observed on: Darwin 27.0.0 (`kern.osrelease`, which each test also logs).

A runner that is not allowed to start `sandbox-exec` leaves the probes out
with `plz test //... --exclude sandbox`.

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

Test: `TestNesting`. `sandbox-exec` runs under an outer profile and starts a
second `sandbox-exec` with an inner profile, which runs `/bin/cat`. Each
profile denies one file of its own:

```scheme
(deny file-read* (literal "<file>"))
```

- E: the inner `sandbox-exec` starts and runs the command: `/bin/cat` of a
  file that neither profile names exits with status 0 and prints the file.
- E: `/bin/cat` of the file the outer profile denies fails with
  `Operation not permitted`.
- E: `/bin/cat` of the file the inner profile denies fails with
  `Operation not permitted`.
- H: an outer profile can refuse the nesting, which then shows as
  `sandbox_apply: Operation not permitted`. Both outer profiles probed start
  from `(allow default)`; which rule refuses it is not probed.

**For plz-mcp u7 to quote:** on macOS a sandbox can be started inside a sandbox
whose profile starts from `(allow default)`, and the process then obeys both
profiles: what either one denies is denied.

Consequence for u5: confinement can be applied by a process that is already
confined. The inner profile can only narrow what the outer one allows.
Consequence for u7: the confined child may start sandboxes of its own; the
port rule of probe 1 still binds them. The proxy runs outside the sandbox,
started before the confined child.

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
- E: a child starts `sandbox-exec` with the base profile and `/usr/bin/true`,
  and it exits with status 0 whether the child is confined or not. Trying to
  start a sandbox does not tell the two cases apart.
- E: so a process cannot tell that it is confined by either means probed.
- H: `sandbox_check(3)` from `libsystem_sandbox` answers the question. It
  needs cgo, which the probes do not use.

Consequence for u5: the code that starts a confined child sets an environment
variable of its own as the marker; nothing else is available without cgo.
Consequence for u7: none.

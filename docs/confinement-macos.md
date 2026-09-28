# Confinement on macOS: what Seatbelt can express

Five probes in `confine/probe_darwin_test.go` start a trivial command through
`/usr/bin/sandbox-exec` under a small profile and assert what happens.

- `E:` is an observation. Each one is an assertion of the named test, so it
  holds on any host where `plz test //confine/...` passes.
- `H:` is an inference that no test asserts.
- Observed on: Darwin 27.0.0 (`kern.osrelease`, which each test also logs).

A runner that is not allowed to start `sandbox-exec` leaves the probes out
with `plz test //... --exclude sandbox`.

The profiles of probes 1 to 5 start with:

```scheme
(version 1)
(allow default)
```

Probe 6 is about the profile that `confine` renders, which starts with
`(deny default)`.

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
second `sandbox-exec` with an inner profile, which runs `/bin/cat` on a file
that no profile names. Two pairs of profiles are probed: both are the base
profile alone, or each adds one rule that denies a file of its own:

```scheme
(deny file-read* (literal "<file>"))
```

- E: when both profiles are the base profile alone, the inner `sandbox-exec`
  starts and the command runs: status 0, and the file is printed.
- E: when each profile denies a file, the inner `sandbox-exec` exits with
  status 71 and does not run the command.
- E: its standard error is then
  `sandbox-exec: sandbox_apply: Operation not permitted`.
- E: the outer profile that denies a file works on its own: the command runs,
  and the denied file is refused. The refusal comes from the nesting.
- H: a profile that is `(allow default)` alone does not count as confinement,
  and any profile that restricts something refuses a second one. The two
  mixed pairs, which would show whether the outer or the inner profile
  decides, are not probed.

**For plz-mcp u7 to quote:** on macOS a sandbox cannot be started inside a
sandbox whose profile restricts anything: `sandbox-exec` exits with status 71
and prints `sandbox_apply: Operation not permitted`. Nesting is refused, not
merged.

Consequence for u5: confinement is applied once, by the process that is not
yet confined. Code that may already run confined must expect the refusal and
must not treat it as a reason to run unconfined.
Consequence for u7: a confined child cannot start sandboxes of its own. The
proxy runs outside the sandbox, started before the confined child.

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
- E: so under the base profile a process cannot tell that it is confined by
  either means probed.
- H: under a profile that restricts something, the attempt to start a sandbox
  is refused as in probe 3, and the refusal is a signal. Probe 3 observes it
  for an inner profile that restricts something too, not for the base profile.
- H: `sandbox_check(3)` from `libsystem_sandbox` answers the question. It
  needs cgo, which the probes do not use.

Consequence for u5: the code that starts a confined child sets an environment
variable of its own as the marker. It is the only signal that is cheap and
does not depend on the profile.
Consequence for u7: none.


## 6. Deny by default

Tests: the escape suite, `confine/escape_test.go` and
`confine/escape_darwin_test.go`, through `confine.Command`. An entry marked
"by hand" was observed with `sandbox-exec` on the same system and no test
asserts it.

```scheme
(version 1)
(deny default)
(allow process-fork)
(allow signal (target same-sandbox))
(allow sysctl-read (sysctl-name-prefix "hw.") (sysctl-name "kern.version"))  ; and more of the machine
(allow file-write* (literal "/dev/null"))
(allow file-read* process-exec (subpath "/System"))   ; and the other system paths
(allow file-read* (literal "/"))
(allow file-read-metadata (literal "/private"))       ; and the links at the root
(allow file-read* process-exec (subpath "<path of the profile>"))
(allow file-write* (subpath "<writable path>"))
(deny file-write* (subpath "<read-only path>"))
(deny file-write* (literal "<writable directory above a read-only path>"))
```

What a program needs to run:

- E: `/bin/sh`, `/bin/cat`, `/bin/ls`, `/bin/mv`, `/usr/bin/env` and a script
  inside the profile run under this profile (the positive cases of the suite).
- E, by hand: without `(allow file-read* (literal "/"))` every program is
  killed as it starts. `sandbox-exec` exits with status 134, which is
  `SIGABRT`, and prints nothing. This is the mark of a profile that denies
  something the dynamic linker needs.
- E, by hand: `/bin/sh` reads `/private/var/select/sh`. `git` and `make` in
  `/usr/bin` read `/private/var/select/developer_dir` and then the developer
  tools, which lie outside the system paths and must be named in the profile.
- E, by hand: `/bin/date` prints UTC unless `/private/var/db/timezone` and
  `/private/etc/localtime` can be read.
- E, by hand: `/Library/Apple` is not needed.
- E, by hand: the Go toolchain runs with its directory named in the profile
  (`go version`, `go env`). It looks at the home directory and goes on when
  that is denied.
- E, by hand: `/usr/bin/id -un` prints the number of the account, not its
  name. The lookup goes to `com.apple.system.opendirectoryd.libinfo`, which
  is denied.

What is refused:

- E: `/bin/cat /etc/passwd` fails (`TestEscapeReadOfASystemFile`).
- E: a canary is not read through its second name under
  `/System/Volumes/Data`, and a read-only path is not written through it
  (`TestEscapeReadThroughTheDataVolume`). Seatbelt matches the path a file
  really has.
- E: `/usr/bin/open -g -a Calculator` fails and no application starts
  (`TestEscapeStartAnApplication`). Under `(allow default)` the application
  starts, outside the sandbox.
- E: `/bin/launchctl list` fails (`TestEscapeAskTheServiceManager`).
- E: a script outside the profile is not executed, and the same script inside
  it is (`TestEscapeRunAProgramOutsideTheProfile`). `process-exec` is allowed
  for the readable paths only.
- E, by hand: a Go program that needs no library is executed from a path it
  cannot be read from when `process-exec` is allowed everywhere. Reading and
  executing are separate operations.
- E, by hand: `/usr/bin/osascript` cannot send an Apple event.

One path inside another:

- E: a later rule overrides an earlier one, as in probe 1, so the rules are
  written from the shortest path to the longest. A read-only path inside a
  writable one cannot be written, created in or moved
  (`TestEscapeWriteToAReadOnlyPathInsideAWritableOne`), and a writable path
  inside a read-only one can be written (`TestWritablePathInsideAReadOnlyOne`).
- E: a rule holds for the path a file has now. With a rule for the read-only
  path alone, a command moves a directory above it, or the writable path
  itself into its temporary directory, and then writes the file under its new
  name. With `(deny file-write* (literal "<directory>"))` for every writable
  directory above a read-only path the move fails, and a file beside the
  read-only path can still be written
  (`TestEscapeMoveADirectoryAboveAReadOnlyPath`).
- E: a path in both lists cannot be written
  (`TestEscapeWriteToAPathThatIsReadOnlyAndWritable`).
- E: a profile with a writable path inside a system path is refused
  (`TestEscapeWritablePathInsideTheSystem`).

The kernel:

- E: with `(allow sysctl-read)` a command reads the table of processes of the
  host, `kern.proc.all`. With the names of the machine and the system alone
  the read is refused, and the programs of the suite, the Go toolchain among
  them, still run (`TestEscapeListTheProcesses`).

Certificates:

- E, by hand: a TLS client in Go fails with `x509: OSStatus -26276` when it
  cannot reach `com.apple.trustd.agent`.
- E, by hand: a certificate can name an address to fetch its issuer from. The
  trust daemon fetches it, from outside the sandbox, when a confined command
  that can reach the daemon verifies such a certificate. The command chooses
  the address, so the daemon is a way around the proxy.
- E, by hand: with `(allow file-read* (subpath "/private/etc/ssl"))` and
  without the daemon, `/usr/bin/curl` fetches `https://proxy.golang.org/`
  through an egress proxy. A client in Go does so with
  `SSL_CERT_FILE=/private/etc/ssl/cert.pem` and still refuses an expired
  certificate. `go mod download` fetches a module the same way.

Consequence for `confine`: the certificates are readable with a proxy and not
without one, and the trust daemon is never in reach. A tool that lives outside
the system paths is named in the profile by the server. The verdict of
`Verify` names the sandbox as "deny by default", so a verdict recorded under
another profile does not hold.

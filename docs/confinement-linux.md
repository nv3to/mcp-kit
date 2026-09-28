# Confinement on Linux: what bubblewrap can do

Findings of the probes in `confine/bwrap_probe_linux_test.go`, which run in
`.github/workflows/linux.yml`. Each probe runs a trivial command through
`bwrap` and either asserts a boundary or records an answer as a line that
starts with `ANSWER:` in the step "Probe log".

An entry marked `E:` was observed: a command was run and this is what it
showed. An entry marked `H:` is expected and has not been observed. Only a
run of the workflow turns an `H:` about the runner into an `E:`.

## Environment

| | Runner | Development machine |
|---|---|---|
| System | H: `ubuntu-24.04`, the image pinned in the workflow | E: macOS, arm64 |
| Kernel | not observed; `TestBwrapProbeEnvironment` and the step "Environment" print it | not applicable |
| bubblewrap | not observed; the step "Install bubblewrap" prints the version | E: not installed, `bwrap` is not on the PATH |

- E: on the development machine `plz test //...` is green and reports
  `//confine:bwrap_probe_test` as `0 passed, 1 skipped`.
- E: the probes compile for Linux (`plz build -a linux_amd64`).
- E: no run of the workflow exists. Every statement about the runner below
  is `H:`.

## Unprivileged user namespaces

bubblewrap, installed without the setuid bit, needs an unprivileged user
namespace. Ubuntu 23.10 and later restrict them through AppArmor
(`kernel.apparmor_restrict_unprivileged_userns = 1`): a program gets one
only if an AppArmor profile allows it.

- H: on the runner the restriction is on and `bwrap` fails with a
  permission error on the first attempt.
- H: lifting the restriction makes `bwrap` start.

The workflow's step "Unprivileged user namespaces" prints the setting, tries
`bwrap`, and lifts the restriction only if that attempt fails. Its log line
that starts with `userns:` says which of the two happened.

**On a real machine a human does this step**; the library never does it and
refuses to run a command while the backend cannot start. Until the next
boot:

```sh
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
```

Permanently:

```sh
echo 'kernel.apparmor_restrict_unprivileged_userns = 0' | sudo tee /etc/sysctl.d/60-bwrap-userns.conf
sudo sysctl --system
```

To check: `bwrap --ro-bind / / --unshare-user --unshare-net true` exits 0.

- Consequence for u6: a backend that cannot start a sandbox is a missing
  backend, the command is refused, and the message names the setting above.
- Consequence for u8: none beyond u6.

## Probe 1: own network namespace

`TestBwrapProbeNetworkNamespace`. The command runs with `--unshare-net` and
dials a listener on the host's `127.0.0.1` and the public address
`1.1.1.1:443`. Controls: the same listener is reached without a sandbox and
from a sandbox that shares the host's network.

- H: both connections fail inside, and the namespace holds `lo` alone.
- The probe fails if either connection succeeds.
- Consequence for u6: `Network: none` is `--unshare-net` and nothing more.
- Consequence for u8: the proxy's address on the host's loopback means
  nothing inside the sandbox; the proxy is reached another way (probes 2
  and 3).

## Probe 2: a unix socket bound into the sandbox

`TestBwrapProbeUnixSocket`. The host listens on a unix socket in a
directory that is bound into a sandbox without network, once with `--bind`
and once with `--ro-bind`.

- H: the socket is reachable through both binds, because connecting to a
  socket is not a write to the filesystem it lies on.
- The probe fails if the read-write bind does not work and records the
  answer for the read-only bind.
- Consequence for u8: the proxy listens on a unix socket in a directory of
  its own, and that directory is bound into the sandbox, read-only if the
  answer allows it.

## Probe 3: a forwarder on the private loopback

`TestBwrapProbeForwarder`. Inside a sandbox without network a forwarder
listens on `127.0.0.1` and relays every connection to the bound socket. A
separate process, an HTTP client with the default transport and
`HTTP_PROXY` as its only configuration, requests `http://outside.probe.test/`.
On the host a stand-in for the proxy serves the socket and fetches from a
server outside.

- H: bubblewrap brings `lo` up in the new namespace, so the forwarder can
  listen and the client can connect.
- H: the client receives the body of the server outside, and a direct
  connection to that server fails.
- The probe fails unless the response arrived through the socket.
- The client must ask for a name that is not a loopback name: an HTTP client
  bypasses its proxy for `localhost` and `127.0.0.1`.

**Mechanism u8 must use to reach the proxy:** the egress proxy listens on a
unix socket; the socket's directory is bound into the sandbox; a forwarder
started inside the sandbox before the command listens on the sandbox's own
`127.0.0.1` and relays to the socket; the command receives `HTTP_PROXY` and
`HTTPS_PROXY` naming the forwarder. The network namespace has no route out,
so the proxy is the only destination.

- Consequence for u6: none.
- Consequence for u8: `confine` is handed the path of a socket, not a TCP
  address, on Linux. The forwarder is a process inside the sandbox, so u8
  needs a program to run there; the probe uses the test binary itself.

## Probe 4: read-only and read-write binds

`TestBwrapProbeBinds`. The root is bound read-only, one directory read-only
and one read-write. The read-write directory holds two symlinks made on the
host, one to a file and one to a directory, both outside it.

- H: a write inside the read-write bind succeeds and reaches the host.
- H: a write in the read-only bind, outside every bind, and through either
  symlink fails, and the directories outside hold nothing afterwards.
- The probe fails if any of the four writes succeeds.
- Consequence for u6: a symlink is resolved inside the sandbox, where its
  target is read-only or absent, so a link cannot widen a profile at run
  time. The profile's paths are still resolved before rendering, because
  `--bind` follows a symlink given as its source.
- Consequence for u8: none.

## Probe 5: whether a process can tell it is confined

`TestBwrapProbeDetect`. The same command reads, outside and inside:
`/proc/self/uid_map`, the name of process 1, the identities of its user,
network and pid namespaces, `NoNewPrivs`, `Seccomp` and `CapEff` from
`/proc/self/status`, and the variable `container`.

- H: `uid_map` differs (one line mapping a single id inside, the full range
  outside), and process 1 is `bwrap` inside.
- H: the namespace identities differ, but a process cannot use them alone:
  it has nothing to compare them with.
- The probe asserts nothing; it records each signal as same or different.
- Consequence for u6: `confine.Confined` on Linux reads the signal the run
  shows to be different and readable without a reference from outside.
  `uid_map` is the candidate. It also differs in any other container, which
  is the safe direction: the answer "confined" makes a server skip nesting.
- Consequence for u8: none.

## Probe 6: bubblewrap inside bubblewrap

`TestBwrapProbeNested`. Inside the sandbox of the other probes `bwrap` is
started again, once with the same namespaces and once with mounts only.

- H: not known. A user namespace may be created inside another, but the
  inner `bwrap` mounts `/proc` and changes the root, which the outer
  sandbox may not permit.
- The probe asserts nothing; it records for each variant whether the inner
  command ran.
- Consequence for u6: `confine` does not nest whatever the answer is. The
  answer decides only the message when a server asks for a confined command
  from inside a sandbox.
- Consequence for u8: none.

## The backend

`confine/bwrap.go` renders a profile to the arguments of `bwrap`, and on
Linux `Command` runs the command through them. The escape suite
(`confine/escape_test.go` and `confine/escape_linux_test.go`) and the tests
of the backend (`confine/bwrap_test.go`) run in the workflow's step "Test".
No run of the workflow exists, so every entry here is `H:`. The rendering
tests of `confine/bwrap_test.go` run on every system against a layout
written into the test; the tests of `Command` through bubblewrap skip where
the system is not Linux.

The arguments, in order: `--unshare-user`, `--unshare-pid`, `--unshare-ipc`,
`--unshare-uts`, `--unshare-cgroup` and `--unshare-net`, never a `-try`
variant, so that a namespace that cannot be unshared stops the start;
`--new-session` and `--die-with-parent`; the binds of the system and of the
profile, `--dev /dev` and `--proc /proc`, from the shortest path to the
longest; `--chdir` to the working directory; then
`-- /usr/bin/env -u PWD` and the command.

- H: the root of the sandbox is an empty file system of its own, and a path
  that is bound appears with the directories that lead to it and nothing
  else of them. `/etc/passwd` is not there, so a read of it fails and
  accounts do not resolve.
- H: `--symlink` makes `/bin`, `/sbin`, `/lib` and `/lib64` the links the
  host has where `/usr` is merged, as on `ubuntu-24.04`, and `/lib64` leads
  to the dynamic loader in `/usr/lib64`.
- H: `--dev /dev` gives the command `null`, `zero`, `full`, `random`,
  `urandom` and `tty`, and `--proc /proc` lists the processes of the
  command's own process namespace, so the process of the test is not in it.
- H: bubblewrap sets `PWD` for the command, after every option that changes
  the environment. `env -u PWD` removes it, so the command gets the
  environment of its profile and nothing else. `env` would take a program
  whose path holds `=` for a variable, so such a program is refused.
- H: a mount point cannot be renamed or removed (`EBUSY`). A writable path
  is a bind, and so is a writable directory above a read-only path, which is
  bound onto itself: none of them can be moved, and a read-only path gets no
  second name. A rename out of a bind into another is refused with `EXDEV`,
  and GNU `mv` then copies and removes; the removal fails on the read-only
  bind, so `mv` fails and the read-only file stays where it is.
- H: a directory inside a read-only bind cannot be moved either, so it is not
  bound onto itself, which would make it writable.
- H: with `Proxy` the command has its own network namespace as with `None`
  (probe 1), so it reaches neither the proxy nor anything else.
- H: `/proc/self/uid_map` reads `0 0 4294967295` outside every user
  namespace and something else inside bubblewrap (probe 5): `Confined` is
  true when it differs or cannot be read.
- H: before every command `Command` starts a sandbox under the empty
  profile. When that start fails, the command is refused with what
  `bwrap` printed and, for each setting of
  [Unprivileged user namespaces](#unprivileged-user-namespaces) that refuses
  them, its name and value. `TestBwrapUsernsRefused` stands in for the
  refusal with a `bwrap` that fails as a refused one does.
- H: the verdict of `Verify` names the version `bwrap --version` prints, that
  the sandbox denies by default, and the release of the kernel, so a verdict
  of another bubblewrap or another kernel does not hold.

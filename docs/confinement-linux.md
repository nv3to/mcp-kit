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

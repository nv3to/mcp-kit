//go:build darwin

package confine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

const sandboxExec = "/usr/bin/sandbox-exec"

// childEnv names the probe the test binary runs instead of the tests. The
// binary is its own confined child, so a probe reports errors first hand.
const childEnv = "MCPKIT_CONFINE_PROBE"

const allowAll = "(version 1)\n(allow default)\n"

const publicAddr = "1.1.1.1:443"

const publicName = "example.com"

const lookupDeadline = 5 * time.Second

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		os.Exit(child(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func child(mode string, args []string) int {
	switch mode {
	case "dial":
		for _, addr := range args {
			fmt.Printf("%s %s\n", addr, dial(addr))
		}
	case "lookup":
		for _, name := range args {
			fmt.Printf("%s %s\n", name, lookup(name))
		}
	case "env":
		env := os.Environ()
		sort.Strings(env)
		for _, kv := range env {
			fmt.Println(kv)
		}
	case "nest":
		fmt.Printf("nest %s\n", nest())
	default:
		fmt.Fprintf(os.Stderr, "unknown probe %q\n", mode)
		return 2
	}
	return 0
}

func dial(addr string) string {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err == nil {
		conn.Close()
		return "ok"
	}
	if errors.Is(err, syscall.EPERM) {
		return "EPERM"
	}
	return "error: " + err.Error()
}

func lookup(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), lookupDeadline)
	defer cancel()
	start := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, name)
	elapsed := time.Since(start).Round(time.Millisecond)
	var dnsErr *net.DNSError
	switch {
	case err == nil:
		return fmt.Sprintf("work after %v: %v", elapsed, addrs)
	case ctx.Err() != nil, errors.As(err, &dnsErr) && dnsErr.IsTimeout:
		return fmt.Sprintf("hang after %v: %v", elapsed, err)
	default:
		return fmt.Sprintf("fail after %v: %v", elapsed, err)
	}
}

func nest() string {
	cmd := exec.Command(sandboxExec, "-p", allowAll, "/usr/bin/true")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return "error: " + err.Error()
	}
	if cmd.ProcessState.ExitCode() == 0 {
		return "started"
	}
	return fmt.Sprintf("refused: exit %d: %s", cmd.ProcessState.ExitCode(), strings.TrimSpace(string(out)))
}

type result struct {
	exit    int
	stdout  string
	stderr  string
	profile string
}

func (r result) dump() string {
	return fmt.Sprintf("exit %d\nstdout: %q\nstderr: %q\nprofile:\n%s", r.exit, r.stdout, r.stderr, r.profile)
}

// outcomes maps the first word of each line the child printed to the second.
func (r result) outcomes() map[string]string {
	got := map[string]string{}
	for _, line := range strings.Split(r.stdout, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 {
			got[fields[0]] = fields[1]
		}
	}
	return got
}

func run(t *testing.T, env []string, argv ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("run %q: %v", argv, err)
	}
	return result{exit: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

func confined(t *testing.T, profile string, env []string, argv ...string) result {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.sb")
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatalf("write the profile: %v", err)
	}
	r := run(t, env, append([]string{sandboxExec, "-f", path}, argv...)...)
	r.profile = profile
	return r
}

// requireUnconfined fails the test when no sandbox can be started at all,
// which is what happens when the test process is itself under a profile.
func requireUnconfined(t *testing.T) {
	t.Helper()
	r := confined(t, allowAll, nil, "/usr/bin/true")
	if r.exit != 0 {
		t.Fatalf("cannot start a sandbox; is this test running inside one?\n%s", r.dump())
	}
	t.Logf("kern.osrelease %s", osRelease())
}

func osRelease() string {
	release, err := syscall.Sysctl("kern.osrelease")
	if err != nil {
		return "unknown: " + err.Error()
	}
	return release
}

func self(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	return path
}

func listen(t *testing.T) (addr, port string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	_, port, err = net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split %q: %v", l.Addr(), err)
	}
	return l.Addr().String(), port
}

func onePort(port string) string {
	return allowAll +
		"(deny network*)\n" +
		"(allow network-outbound (remote ip \"localhost:" + port + "\"))\n"
}

func TestLoopbackPort(t *testing.T) {
	requireUnconfined(t)
	allowed, port := listen(t)
	other, _ := listen(t)
	env := []string{childEnv + "=dial"}

	control := run(t, env, self(t), allowed, other)
	if got := control.outcomes(); got[allowed] != "ok" || got[other] != "ok" {
		t.Fatalf("unconfined dials failed, the probe proves nothing\n%s", control.dump())
	}

	r := confined(t, onePort(port), env, self(t), allowed, other, publicAddr)
	got := r.outcomes()
	if got[allowed] != "ok" {
		t.Errorf("dial to the allowed port %s: got %q, want ok", allowed, got[allowed])
	}
	if got[other] != "EPERM" {
		t.Errorf("dial to the other loopback port %s: got %q, want EPERM", other, got[other])
	}
	if got[publicAddr] != "EPERM" {
		t.Errorf("dial to the public address %s: got %q, want EPERM", publicAddr, got[publicAddr])
	}
	if t.Failed() {
		t.Log(r.dump())
	}
}

func TestNameResolution(t *testing.T) {
	requireUnconfined(t)
	_, port := listen(t)
	env := []string{childEnv + "=lookup"}

	control := run(t, env, self(t), publicName)
	if got := control.outcomes(); got[publicName] != "work" {
		t.Fatalf("unconfined lookup failed, the probe proves nothing\n%s", control.dump())
	}

	r := confined(t, onePort(port), env, self(t), publicName)
	if got := r.outcomes(); got[publicName] != "fail" {
		t.Errorf("confined lookup of %s: got %q, want fail\n%s", publicName, got[publicName], r.dump())
	}
}

func TestNesting(t *testing.T) {
	requireUnconfined(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve the fixture directory: %v", err)
	}
	free := filepath.Join(dir, "free.txt")
	outerDenied := filepath.Join(dir, "outer.txt")
	innerDenied := filepath.Join(dir, "inner.txt")
	for _, path := range []string{free, outerDenied, innerDenied} {
		if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	outer := allowAll + fmt.Sprintf("(deny file-read* (literal %q))\n", outerDenied)
	inner := filepath.Join(dir, "inner.sb")
	innerProfile := allowAll + fmt.Sprintf("(deny file-read* (literal %q))\n", innerDenied)
	if err := os.WriteFile(inner, []byte(innerProfile), 0o600); err != nil {
		t.Fatalf("write the inner profile: %v", err)
	}
	nested := func(path string) result {
		r := confined(t, outer, nil, sandboxExec, "-f", inner, "/bin/cat", path)
		r.profile += "inner profile:\n" + innerProfile
		return r
	}

	if r := nested(free); r.exit != 0 || r.stdout != "content" {
		t.Errorf("nested sandbox: want it to start and run the command\n%s", r.dump())
	}
	for _, path := range []string{outerDenied, innerDenied} {
		r := nested(path)
		if r.exit == 0 || !strings.Contains(r.stderr, "Operation not permitted") {
			t.Errorf("read of %s under both profiles: want it refused with EPERM\n%s", path, r.dump())
		}
	}
}

// accountHome is the home directory of the account. plz points HOME at the
// test's own directory and unsets USER, so the shell looks it up.
func accountHome(t *testing.T) string {
	t.Helper()
	name := run(t, nil, "/usr/bin/id", "-un")
	if name.exit != 0 {
		t.Fatalf("look up the account name\n%s", name.dump())
	}
	tilde := "~" + strings.TrimSpace(name.stdout)
	r := run(t, nil, "/bin/sh", "-c", "echo "+tilde)
	home := strings.TrimSpace(r.stdout)
	if r.exit != 0 || !filepath.IsAbs(home) {
		t.Fatalf("expand %s\n%s", tilde, r.dump())
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatalf("resolve the home directory: %v", err)
	}
	return home
}

func TestHomeReads(t *testing.T) {
	requireUnconfined(t)
	home := accountHome(t)
	dir, err := os.MkdirTemp(home, ".mcpkit-confine-probe-")
	if err != nil {
		t.Fatalf("create the fixture under the home directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	allowedDir := filepath.Join(dir, "allowed")
	inside := filepath.Join(allowedDir, "inside.txt")
	outside := filepath.Join(dir, "outside.txt")
	link := filepath.Join(allowedDir, "link.txt")
	if err := os.Mkdir(allowedDir, 0o700); err != nil {
		t.Fatalf("create the allowed directory: %v", err)
	}
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatalf("write %s: %v", inside, err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write %s: %v", outside, err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("link %s: %v", link, err)
	}

	profile := allowAll +
		fmt.Sprintf("(deny file-read* (subpath %q))\n", home) +
		fmt.Sprintf("(allow file-read* (subpath %q))\n", allowedDir)

	if r := run(t, nil, "/bin/cat", link); r.exit != 0 || r.stdout != "outside" {
		t.Fatalf("unconfined read through the symlink failed, the probe proves nothing\n%s", r.dump())
	}
	if r := confined(t, profile, nil, "/bin/cat", inside); r.exit != 0 || r.stdout != "inside" {
		t.Errorf("read inside the allowed directory: want it to succeed\n%s", r.dump())
	}
	for _, path := range []string{outside, link} {
		r := confined(t, profile, nil, "/bin/cat", path)
		if r.exit == 0 || !strings.Contains(r.stderr, "Operation not permitted") {
			t.Errorf("read of %s: want it refused with EPERM\n%s", path, r.dump())
		}
	}
}

func TestDetection(t *testing.T) {
	requireUnconfined(t)

	env := []string{childEnv + "=env"}
	free := run(t, env, self(t))
	held := confined(t, allowAll, env, self(t))
	if free.exit != 0 || held.exit != 0 || free.stdout != held.stdout {
		t.Errorf("environment: want the same inside and outside\noutside: %s\ninside: %s", free.dump(), held.dump())
	}

	env = []string{childEnv + "=nest"}
	free = run(t, env, self(t))
	if got := free.outcomes(); got["nest"] != "started" {
		t.Errorf("unconfined process starting a sandbox: got %q, want started\n%s", got["nest"], free.dump())
	}
	held = confined(t, allowAll, env, self(t))
	if got := held.outcomes(); got["nest"] != "started" {
		t.Errorf("confined process starting a sandbox: got %q, want started\n%s", got["nest"], held.dump())
	}
}

//go:build linux

package confine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The probes run this test binary inside bubblewrap as their trivial
// command: roleEnv selects what it does there, so no probe depends on a
// tool the runner may not have.
const (
	roleEnv   = "BWRAP_PROBE_ROLE"
	argEnv    = "BWRAP_PROBE_ARG"
	helperRun = "-test.run=^TestBwrapProbeHelper$"

	// The name the HTTP client asks for. It is not a loopback name, so the
	// client does not bypass the proxy for it.
	outsideHost = "outside.probe.test"
)

// TestBwrapProbeHelper is the command the probes confine. It is skipped
// unless a probe started it.
func TestBwrapProbeHelper(t *testing.T) {
	role := os.Getenv(roleEnv)
	if role == "" {
		t.Skip("helper of the bubblewrap probes; started by them inside the sandbox")
	}
	arg := os.Getenv(argEnv)
	var err error
	switch role {
	case "noop":
		fmt.Println("ran")
	case "dial":
		network, address, _ := strings.Cut(arg, ",")
		var c net.Conn
		if c, err = net.DialTimeout(network, address, 3*time.Second); err == nil {
			c.Close()
		}
	case "read":
		network, address, _ := strings.Cut(arg, ",")
		var c net.Conn
		if c, err = net.DialTimeout(network, address, 3*time.Second); err == nil {
			var b []byte
			b, err = io.ReadAll(c)
			fmt.Printf("%s", b)
			c.Close()
		}
	case "write":
		err = os.WriteFile(arg, []byte("written from inside"), 0o644)
	case "signals":
		fmt.Print(signals())
	case "interfaces":
		fmt.Print(interfaces())
	case "forward":
		err = forwardAndGet(arg)
	case "get":
		err = get(arg)
	default:
		err = fmt.Errorf("unknown role %q", role)
	}
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// forwardAndGet listens on the sandbox's own loopback, relays every
// connection to the unix socket, and starts the HTTP client as a separate
// process that knows the forwarder only through HTTP_PROXY.
func forwardAndGet(socket string) error {
	fmt.Print(interfaces())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on the private loopback: %w", err)
	}
	defer l.Close()
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer in.Close()
				out, err := net.Dial("unix", socket)
				if err != nil {
					return
				}
				defer out.Close()
				go io.Copy(out, in)
				io.Copy(in, out)
			}()
		}
	}()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	client := exec.Command(exe, helperRun)
	client.Env = []string{
		roleEnv + "=get",
		argEnv + "=http://" + outsideHost + "/",
		"HTTP_PROXY=http://" + l.Addr().String(),
	}
	client.Stdout = os.Stdout
	client.Stderr = os.Stderr
	return client.Run()
}

// get is the ordinary HTTP client: the default transport, which takes its
// proxy from the environment.
func get(url string) error {
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("status=%d body=%s\n", resp.StatusCode, body)
	return nil
}

// signals prints what a process can read about itself that may differ
// inside the sandbox, one name=value per line.
func signals() string {
	var b strings.Builder
	file := func(name, path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(&b, "%s=unreadable: %v\n", name, err)
			return
		}
		value := strings.ReplaceAll(strings.TrimSpace(string(data)), "\x00", " ")
		fmt.Fprintf(&b, "%s=%s\n", name, strings.Join(strings.Fields(value), " "))
	}
	file("uid_map", "/proc/self/uid_map")
	file("pid1_comm", "/proc/1/comm")
	link := func(name, path string) {
		target, err := os.Readlink(path)
		if err != nil {
			fmt.Fprintf(&b, "%s=unreadable: %v\n", name, err)
			return
		}
		fmt.Fprintf(&b, "%s=%s\n", name, target)
	}
	link("userns", "/proc/self/ns/user")
	link("netns", "/proc/self/ns/net")
	link("pidns", "/proc/self/ns/pid")
	status, _ := os.ReadFile("/proc/self/status")
	for _, line := range strings.Split(string(status), "\n") {
		for _, key := range []string{"NoNewPrivs", "Seccomp", "CapEff"} {
			if value, ok := strings.CutPrefix(line, key+":"); ok {
				fmt.Fprintf(&b, "%s=%s\n", key, strings.TrimSpace(value))
			}
		}
	}
	fmt.Fprintf(&b, "env_container=%s\n", os.Getenv("container"))
	return b.String()
}

func interfaces() string {
	var b strings.Builder
	list, err := net.Interfaces()
	if err != nil {
		return fmt.Sprintf("interfaces=unreadable: %v\n", err)
	}
	for _, i := range list {
		fmt.Fprintf(&b, "interface=%s flags=%s\n", i.Name, i.Flags)
	}
	return b.String()
}

// base is the sandbox every probe starts from: the whole filesystem read
// only, fresh /dev and /proc, and its own user, pid and network namespaces.
var base = []string{
	"--ro-bind", "/", "/",
	"--dev", "/dev",
	"--proc", "/proc",
	"--unshare-user",
	"--unshare-pid",
	"--unshare-net",
	"--die-with-parent",
}

// inside runs the helper in a given role inside bubblewrap and returns what
// it printed and whether it succeeded.
func inside(t *testing.T, bwrapArgs []string, role, arg string) (string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{}, bwrapArgs...), "--", exe, helperRun)
	cmd := exec.Command("bwrap", args...)
	cmd.Env = []string{roleEnv + "=" + role, argEnv + "=" + arg}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// outside runs the helper in the same role without a sandbox, as the
// control of a probe.
func outside(t *testing.T, role, arg string) (string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, helperRun)
	cmd.Env = []string{roleEnv + "=" + role, argEnv + "=" + arg}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// requireBwrap fails, never skips, when bubblewrap is missing or cannot
// start: on Linux a skipped probe would be a green run that shows nothing.
func requireBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Fatalf("bubblewrap is not installed: %v", err)
	}
	if out, err := inside(t, base, "noop", ""); err != nil {
		t.Fatalf("bubblewrap cannot start a sandbox (unprivileged user namespaces refused?): %v\n%s", err, out)
	}
}

// shortDir is a temporary directory with a short path: a unix socket's path
// is limited to 107 bytes and the test's own directory may be longer.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bwp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func answer(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("ANSWER: "+format, args...)
}

// The environment of the run, so that the log of a run is the evidence on
// its own.
func TestBwrapProbeEnvironment(t *testing.T) {
	for _, path := range []string{
		"/proc/sys/kernel/osrelease",
		"/proc/sys/kernel/apparmor_restrict_unprivileged_userns",
		"/proc/sys/kernel/unprivileged_userns_clone",
		"/proc/sys/user/max_user_namespaces",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Logf("%s: %v", path, err)
			continue
		}
		t.Logf("%s: %s", path, strings.TrimSpace(string(data)))
	}
	release, _ := os.ReadFile("/etc/os-release")
	for _, line := range strings.Split(string(release), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			t.Log(line)
		}
	}
	version, err := exec.Command("bwrap", "--version").CombinedOutput()
	t.Logf("bwrap --version: %s (%v)", strings.TrimSpace(string(version)), err)
	requireBwrap(t)
	answer(t, "bubblewrap starts a sandbox with its own user, pid and network namespaces")
}

// Probe 1: in its own network namespace a command reaches neither a public
// address nor the host's loopback.
func TestBwrapProbeNetworkNamespace(t *testing.T) {
	requireBwrap(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	loopback := "tcp," + l.Addr().String()
	const public = "tcp,1.1.1.1:443"

	if out, err := outside(t, "dial", loopback); err != nil {
		t.Fatalf("control: the host's loopback listener is not reachable without a sandbox: %v\n%s", err, out)
	}
	shared := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--unshare-user", "--unshare-pid"}
	if out, err := inside(t, shared, "dial", loopback); err != nil {
		t.Fatalf("control: the host's loopback is not reachable from a sandbox that shares the network: %v\n%s", err, out)
	}
	out, err := outside(t, "dial", public)
	t.Logf("control: %s without a sandbox: err=%v %s", public, err, out)

	out, err = inside(t, base, "dial", loopback)
	if err == nil {
		t.Errorf("the host's loopback %s was reached from its own network namespace", loopback)
	}
	t.Logf("host loopback from inside: %s", out)
	out, err = inside(t, base, "dial", public)
	if err == nil {
		t.Errorf("the public address %s was reached from its own network namespace", public)
	}
	t.Logf("public address from inside: %s", out)

	out, _ = inside(t, base, "interfaces", "")
	t.Logf("interfaces inside:\n%s", out)
	if !t.Failed() {
		answer(t, "with --unshare-net neither a public address nor the host's loopback is reachable")
	}
}

// Probe 2: a unix socket bound into the sandbox is reachable from inside,
// although the sandbox has no network.
func TestBwrapProbeUnixSocket(t *testing.T) {
	requireBwrap(t)
	dir := shortDir(t)
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "from the host")
			c.Close()
		}
	}()

	reached := map[string]bool{}
	for _, bind := range []string{"--bind", "--ro-bind"} {
		out, err := inside(t, append(append([]string{}, base...), bind, dir, dir), "read", "unix,"+socket)
		reached[bind] = err == nil && out == "from the host"
		t.Logf("%s: err=%v output=%q", bind, err, out)
	}
	if !reached["--bind"] {
		t.Errorf("a unix socket bound read-write into the sandbox is not reachable from inside")
	}
	answer(t, "a unix socket is reachable from inside: read-write bind %v, read-only bind %v",
		reached["--bind"], reached["--ro-bind"])
}

// Probe 3: a forwarder on the sandbox's private loopback relays to the unix
// socket, and an HTTP client that knows only HTTP_PROXY reaches a server
// outside through it.
func TestBwrapProbeForwarder(t *testing.T) {
	requireBwrap(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "from the server outside")
	}))
	defer server.Close()

	// The stand-in for the egress proxy: it listens on the unix socket and
	// fetches from the server outside what a client asks of outsideHost.
	dir := shortDir(t)
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	proxied := make(chan string, 8)
	proxy := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied <- r.Method + " " + r.RequestURI
		if r.URL.Host != outsideHost {
			http.Error(w, "not a proxy request for "+outsideHost, http.StatusBadGateway)
			return
		}
		resp, err := http.Get(server.URL + r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})}
	go proxy.Serve(l)
	defer proxy.Close()

	out, err := inside(t, append(append([]string{}, base...), "--bind", dir, dir), "forward", socket)
	t.Logf("inside: err=%v\n%s", err, out)
	if err != nil || !strings.Contains(out, "status=200 body=from the server outside") {
		t.Fatalf("the client inside did not reach the server outside through the forwarder")
	}
	select {
	case request := <-proxied:
		t.Logf("the proxy on the socket saw: %s", request)
	default:
		t.Fatalf("the response did not come through the proxy on the socket")
	}
	if out, err := inside(t, base, "dial", "tcp,"+server.Listener.Addr().String()); err == nil {
		t.Errorf("the server outside was reached directly, not through the proxy: %s", out)
	}
	if !t.Failed() {
		answer(t, "the private loopback is usable, and a forwarder on it to a bound unix socket carries an HTTP client configured by HTTP_PROXY alone to a server outside")
	}
}

// Probe 4: a write succeeds inside a read-write bind and fails everywhere
// else, including through a symlink that leaves the read-write bind.
func TestBwrapProbeBinds(t *testing.T) {
	requireBwrap(t)
	dir := shortDir(t)
	ro, rw, out := filepath.Join(dir, "ro"), filepath.Join(dir, "rw"), filepath.Join(dir, "out")
	for _, d := range []string{ro, rw, out} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(out, "by-file-link"), filepath.Join(rw, "file-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(rw, "dir-link")); err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{}, base...), "--ro-bind", ro, ro, "--bind", rw, rw)

	allowed := filepath.Join(rw, "allowed")
	if o, err := inside(t, args, "write", allowed); err != nil {
		t.Fatalf("control: a write inside the read-write bind failed: %v\n%s", err, o)
	}
	if _, err := os.Stat(allowed); err != nil {
		t.Fatalf("control: the write inside the read-write bind did not reach the host: %v", err)
	}

	for name, path := range map[string]string{
		"in the read-only bind":            filepath.Join(ro, "f"),
		"outside every bind":               filepath.Join(out, "f"),
		"through a symlink to a file":      filepath.Join(rw, "file-link"),
		"through a symlink to a directory": filepath.Join(rw, "dir-link", "f"),
	} {
		o, err := inside(t, args, "write", path)
		if err == nil {
			t.Errorf("a write %s succeeded: %s", name, path)
		}
		t.Logf("write %s: %s", name, o)
	}
	for _, d := range []string{ro, out} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			t.Errorf("%s holds %s after the refused writes", d, e.Name())
		}
	}
	if !t.Failed() {
		answer(t, "a write lands only inside a read-write bind; a symlink out of it is resolved inside the sandbox and the write is refused")
	}
}

// Probe 5: what a process can read about itself that differs inside the
// sandbox. It records the answer and asserts nothing about which it is.
func TestBwrapProbeDetect(t *testing.T) {
	requireBwrap(t)
	parse := func(text string) map[string]string {
		m := map[string]string{}
		for _, line := range strings.Split(text, "\n") {
			if name, value, ok := strings.Cut(line, "="); ok {
				m[name] = value
			}
		}
		return m
	}
	hostText, err := outside(t, "signals", "")
	if err != nil {
		t.Fatalf("signals without a sandbox: %v\n%s", err, hostText)
	}
	insideText, err := inside(t, base, "signals", "")
	if err != nil {
		t.Fatalf("signals inside: %v\n%s", err, insideText)
	}
	host, in := parse(hostText), parse(insideText)
	var differ []string
	for _, name := range []string{"uid_map", "pid1_comm", "userns", "netns", "pidns", "NoNewPrivs", "Seccomp", "CapEff", "env_container"} {
		mark := "same"
		if host[name] != in[name] {
			mark = "DIFFERS"
			differ = append(differ, name)
		}
		t.Logf("%-14s %-8s outside=%q inside=%q", name, mark, host[name], in[name])
	}
	if len(differ) == 0 {
		answer(t, "a process cannot tell it is confined from any signal probed")
		return
	}
	answer(t, "a process can tell it is confined; the signals that differ: %s", strings.Join(differ, ", "))
}

// Probe 6: bubblewrap started inside bubblewrap. It records whether that
// works and asserts nothing about which it is.
func TestBwrapProbeNested(t *testing.T) {
	requireBwrap(t)
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]bool{}
	for name, inner := range map[string][]string{
		"same namespaces": base,
		"mounts only":     {"--ro-bind", "/", "/"},
	} {
		args := append(append([]string{}, base...), "--", bwrap)
		args = append(append(args, inner...), "--", exe, helperRun)
		cmd := exec.Command(bwrap, args...)
		cmd.Env = []string{roleEnv + "=noop"}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("inner sandbox with %s: %v", name, err)
		}
		results[name] = err == nil && strings.TrimSpace(string(out)) == "ran"
		t.Logf("inner sandbox with %s: err=%v output=%q", name, err, strings.TrimSpace(string(out)))
	}
	answer(t, "bubblewrap inside bubblewrap: with the same namespaces %v, with mounts only %v",
		results["same namespaces"], results["mounts only"])
}

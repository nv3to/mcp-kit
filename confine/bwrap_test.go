package confine

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	mcpkit "github.com/nv3to/mcp-kit"
)

func refusal(err error) bool {
	var e *mcpkit.Error
	return errors.As(err, &e) && e.Kind == mcpkit.Refused
}

// bwrapMerged is the layout of a system where /usr is merged, as on Debian
// and Ubuntu.
var bwrapMerged = []bwrapEntry{
	{path: "/bin", link: "usr/bin"},
	{path: "/lib", link: "usr/lib"},
	{path: "/lib64", link: "usr/lib64"},
	{path: "/sbin", link: "usr/sbin"},
	{path: "/usr/bin"},
	{path: "/usr/lib"},
	{path: "/usr/lib64"},
	{path: "/usr/libexec"},
	{path: "/usr/sbin"},
	{path: "/usr/share"},
	{path: "/etc/alternatives"},
	{path: "/etc/ld.so.cache"},
	{path: "/etc/localtime", link: "/usr/share/zoneinfo/Etc/UTC"},
	{path: "/etc/ssl/cert.pem", link: "certs/ca-certificates.crt", certificate: true},
	{path: "/etc/ssl/certs", certificate: true},
}

// The forwarder, the socket of its relay and the proxy of the rendering tests.
const (
	testForwarder = "/opt/server/bin/server"
	testSocket    = "/tmp/confine-proxy-1/proxy"
	testProxy     = "127.0.0.1:3128"
)

func mustArgs(t *testing.T, r resolved, argv ...string) []string {
	t.Helper()
	args, err := bwrapArgs("/usr/bin/bwrap", testForwarder, bwrapMerged, r, argv)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return args
}

// step returns where the option with its operands stands in args, or -1.
func step(args []string, option string, operands ...string) int {
	for i := range args {
		start, end := i+1, i+1+len(operands)
		if args[i] == option && end <= len(args) && slices.Equal(args[start:end], operands) {
			return i
		}
	}
	return -1
}

func TestBwrapRender(t *testing.T) {
	r := resolved{
		readOnly:  []string{"/opt/read-only", "/opt/read-write/above/kept"},
		readWrite: []string{"/opt/read-write"},
		tmp:       "/tmp/confine-1",
		dir:       "/opt/read-only",
	}
	args := mustArgs(t, r, "/bin/cat", "file")
	if again := mustArgs(t, r, "/bin/cat", "file"); !reflect.DeepEqual(args, again) {
		t.Errorf("render is not a function of the profile:\n%q\n%q", args, again)
	}
	line := strings.Join(args, " ")

	const start = "/usr/bin/bwrap --unshare-user --unshare-pid --unshare-ipc --unshare-uts --unshare-cgroup --unshare-net --new-session --die-with-parent "
	if !strings.HasPrefix(line, start) {
		t.Errorf("the sandbox does not start with every namespace of its own:\n%s", line)
	}
	const end = " --chdir /opt/read-only -- /usr/bin/env -u PWD /bin/cat file"
	if !strings.HasSuffix(line, end) {
		t.Errorf("the command does not end with %q:\n%s", end, line)
	}
	for _, want := range [][]string{
		{"--dev", "/dev"},
		{"--proc", "/proc"},
		{"--symlink", "usr/bin", "/bin"},
		{"--symlink", "/usr/share/zoneinfo/Etc/UTC", "/etc/localtime"},
		{"--ro-bind", "/usr/bin", "/usr/bin"},
		{"--ro-bind", "/usr/lib", "/usr/lib"},
		{"--ro-bind", "/etc/ld.so.cache", "/etc/ld.so.cache"},
		{"--ro-bind", "/opt/read-only", "/opt/read-only"},
		{"--ro-bind", "/opt/read-write/above/kept", "/opt/read-write/above/kept"},
		{"--bind", "/opt/read-write", "/opt/read-write"},
		{"--bind", "/tmp/confine-1", "/tmp/confine-1"},
	} {
		if step(args, want[0], want[1:]...) < 0 {
			t.Errorf("the sandbox lacks %q:\n%s", want, line)
		}
	}
	for _, path := range []string{"/", "/etc", "/etc/ssl/certs", "/usr", "/usr/local", "/home", "/root", "/opt", "/var", "/tmp", "/run", testForwarder} {
		for _, option := range []string{"--bind", "--ro-bind"} {
			if step(args, option, path, path) >= 0 {
				t.Errorf("the sandbox binds %s:\n%s", path, line)
			}
		}
	}
	if strings.Contains(line, "-try") || strings.Contains(line, "--share-net") {
		t.Errorf("the sandbox may share a namespace with the host:\n%s", line)
	}

	outer := step(args, "--bind", "/opt/read-write", "/opt/read-write")
	above := step(args, "--bind", "/opt/read-write/above", "/opt/read-write/above")
	inner := step(args, "--ro-bind", "/opt/read-write/above/kept", "/opt/read-write/above/kept")
	if outer < 0 || above < 0 || inner < 0 || !(outer < above && above < inner) {
		t.Errorf("the writable path, the directory above the read-only path and the read-only path are bound at %d, %d and %d, want in that order:\n%s", outer, above, inner, line)
	}
	if got := strings.Count(line, "--bind "); got != 3 {
		t.Errorf("the sandbox has %d writable binds, want the writable path, the directory above the read-only path and the temporary directory:\n%s", got, line)
	}
}

func TestBwrapRenderReadOnlyWins(t *testing.T) {
	args := mustArgs(t, resolved{readOnly: []string{"/opt/both"}, readWrite: []string{"/opt/both"}}, "/bin/cat")
	writable := step(args, "--bind", "/opt/both", "/opt/both")
	readOnly := step(args, "--ro-bind", "/opt/both", "/opt/both")
	if writable < 0 || readOnly < writable {
		t.Errorf("a path in both lists is bound writable at %d and read-only at %d, want the read-only bind later:\n%q", writable, readOnly, args)
	}
}

func TestBwrapRenderHeld(t *testing.T) {
	// Inside a read-only path a directory cannot be moved anyway, and a bind
	// onto itself would make it writable.
	args := mustArgs(t, resolved{
		readOnly:  []string{"/opt/kept", "/opt/kept/deep/kept"},
		readWrite: []string{"/opt"},
	}, "/bin/cat")
	if step(args, "--bind", "/opt/kept/deep", "/opt/kept/deep") >= 0 {
		t.Errorf("/opt/kept/deep lies inside a read-only path and is bound writable:\n%q", args)
	}
	// Inside a writable path that lies inside a read-only one, it is held.
	args = mustArgs(t, resolved{
		readOnly:  []string{"/opt/kept", "/opt/kept/out/above/kept"},
		readWrite: []string{"/opt/kept/out"},
	}, "/bin/cat")
	if step(args, "--bind", "/opt/kept/out/above", "/opt/kept/out/above") < 0 {
		t.Errorf("the writable directory above a read-only path is not held:\n%q", args)
	}
	if step(args, "--bind", "/opt/kept", "/opt/kept") >= 0 || step(args, "--bind", "/opt", "/opt") >= 0 {
		t.Errorf("a directory outside the writable path is bound writable:\n%q", args)
	}
}

func TestBwrapRenderNetwork(t *testing.T) {
	closed := mustArgs(t, resolved{}, "/bin/cat", "file")
	narrowed := mustArgs(t, resolved{proxy: netip.MustParseAddrPort(testProxy), relay: testSocket}, "/bin/cat", "file")
	for _, args := range [][]string{closed, narrowed} {
		if step(args, "--unshare-net") < 0 {
			t.Errorf("the network is no namespace of its own:\n%q", args)
		}
	}

	const direct = " -- /usr/bin/env -u PWD /bin/cat file"
	const forwarded = " -- /usr/bin/env -u PWD " + testForwarder + " " + forwardArg + " " + testProxy + " " + testSocket + " /bin/cat file"
	if line := strings.Join(closed, " "); !strings.HasSuffix(line, direct) {
		t.Errorf("with the network closed the command does not end with %q:\n%s", direct, line)
	}
	if line := strings.Join(narrowed, " "); !strings.HasSuffix(line, forwarded) {
		t.Errorf("with a proxy the forwarder does not start the command, want the end %q:\n%s", forwarded, line)
	}

	bindings := [][]string{
		{"--ro-bind", testForwarder, testForwarder},
		{"--ro-bind", "/tmp/confine-proxy-1", "/tmp/confine-proxy-1"},
		{"--ro-bind", "/etc/ssl/certs", "/etc/ssl/certs"},
		{"--symlink", "certs/ca-certificates.crt", "/etc/ssl/cert.pem"},
	}
	for _, want := range bindings {
		if step(narrowed, want[0], want[1:]...) < 0 {
			t.Errorf("with a proxy the sandbox lacks %q:\n%q", want, narrowed)
		}
		if step(closed, want[0], want[1:]...) >= 0 {
			t.Errorf("with the network closed the sandbox has %q:\n%q", want, closed)
		}
	}
	if step(narrowed, "--bind", "/tmp/confine-proxy-1", "/tmp/confine-proxy-1") >= 0 {
		t.Errorf("the directory of the socket is writable:\n%q", narrowed)
	}

	for _, r := range []resolved{
		{proxy: netip.MustParseAddrPort(testProxy)},
		{proxy: netip.MustParseAddrPort(testProxy), relay: testSocket},
	} {
		forwarder := testForwarder
		if r.relay != "" {
			forwarder = ""
		}
		if args, err := bwrapArgs("/usr/bin/bwrap", forwarder, bwrapMerged, r, []string{"/bin/cat"}); err == nil {
			t.Errorf("a proxy with the forwarder %q and the socket %q: want it refused, got %q", forwarder, r.relay, args)
		}
	}
}

func TestBwrapCannotExpress(t *testing.T) {
	for _, r := range []resolved{
		{readWrite: []string{"/usr/share"}},
		{readWrite: []string{"/usr/lib/x86_64-linux-gnu"}},
		{readWrite: []string{"/etc/alternatives"}},
		{readWrite: []string{"/etc/ssl/certs"}},
		{readWrite: []string{"/bin"}},
	} {
		if args, err := bwrapArgs("/usr/bin/bwrap", "", bwrapMerged, r, []string{"/bin/cat"}); err == nil {
			t.Errorf("a writable path inside the system %q: want it refused, got %q", r.readWrite, args)
		}
	}
	if args, err := bwrapArgs("/usr/bin/bwrap", "", bwrapMerged, resolved{}, []string{"/opt/a=b/program"}); err == nil {
		t.Errorf("a program with an equals sign: want it refused, got %q", args)
	}
	narrowed := resolved{proxy: netip.MustParseAddrPort(testProxy), relay: testSocket}
	if args, err := bwrapArgs("/usr/bin/bwrap", "/opt/a=b/server", bwrapMerged, narrowed, []string{"/bin/cat"}); err == nil {
		t.Errorf("a forwarder with an equals sign: want it refused, got %q", args)
	}
}

// go test links the escape suite, which calls Init, into this binary; the
// flag is cleared for the test.
func TestBwrapProxyNeedsInit(t *testing.T) {
	called := initialized.Swap(false)
	t.Cleanup(func() { initialized.Store(called) })
	if _, err := forwarder(); err == nil || !strings.Contains(err.Error(), "confine.Init") {
		t.Errorf("the forwarder without Init: got %v, want an error that names confine.Init", err)
	}
	requireLinux(t)
	cmd, err := Command(context.Background(), Profile{Network: Proxy(testProxy)}, "/usr/bin/true")
	if !refusal(err) || cmd != nil || !strings.Contains(err.Error(), "confine.Init") {
		t.Errorf("Command with a proxy and without Init: got %v, want an error of kind refused that names confine.Init, and no command", err)
	}
}

// A stand-in for the proxy echoes what it receives.
func echo(t *testing.T) netip.AddrPort {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c)
				c.Close()
			}()
		}
	}()
	return netip.MustParseAddrPort(l.Addr().String())
}

func TestRelay(t *testing.T) {
	r, err := startRelay(echo(t))
	if err != nil {
		t.Fatalf("start the relay: %v", err)
	}
	defer r.close()
	if len(r.socket) > socketMax || filepath.Dir(r.socket) != r.dir {
		t.Errorf("the socket is %s, want at most %d bytes in the directory %s", r.socket, socketMax, r.dir)
	}

	// A half close reaches the proxy, and its answer comes back.
	c, err := net.Dial("unix", r.socket)
	if err != nil {
		t.Fatalf("connect to the relay: %v", err)
	}
	defer c.Close()
	io.WriteString(c, "through the relay")
	c.(*net.UnixConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(c); err != nil || string(got) != "through the relay" {
		t.Errorf("the answer through the relay: got %v and %q, want the echo", err, got)
	}

	// A connection that stays open ends with the relay, and nothing is left.
	held, err := net.Dial("unix", r.socket)
	if err != nil {
		t.Fatalf("connect to the relay: %v", err)
	}
	defer held.Close()
	io.WriteString(held, "x")
	held.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(held, make([]byte, 1)); err != nil {
		t.Fatalf("the echo through the relay: %v", err)
	}
	if err := r.close(); err != nil {
		t.Errorf("close the relay: %v", err)
	}
	if n, err := held.Read(make([]byte, 1)); err == nil {
		t.Errorf("a connection through the closed relay read %d bytes, want its end", n)
	}
	if _, err := os.Lstat(r.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory %s of the closed relay: got %v, want it removed", r.dir, err)
	}
}

func TestRelayWithADeepTemporaryDirectory(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", socketMax))
	if err := os.Mkdir(deep, 0o700); err != nil {
		t.Fatalf("create %s: %v", deep, err)
	}
	t.Setenv("TMPDIR", deep)
	r, err := startRelay(echo(t))
	if err != nil {
		t.Fatalf("start the relay: %v", err)
	}
	defer r.close()
	if len(r.socket) > socketMax || strings.HasPrefix(r.socket, deep) {
		t.Errorf("the socket is %s, want at most %d bytes outside %s", r.socket, socketMax, deep)
	}
}

// fakeBwrap puts a bubblewrap on the PATH that fails as one does when the
// system refuses it a user namespace, and settings of the kernel beside it.
func fakeBwrap(t *testing.T, restrict string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'bwrap: setting up uid map: Permission denied' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "bwrap"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	sysctl := t.TempDir()
	if err := os.Mkdir(filepath.Join(sysctl, "kernel"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysctl, "kernel", "apparmor_restrict_unprivileged_userns"), []byte(restrict+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := bwrapSysctl
	bwrapSysctl = sysctl
	t.Cleanup(func() { bwrapSysctl = found })
}

func TestBwrapCannotStart(t *testing.T) {
	fakeBwrap(t, "1")
	err := (bubblewrap{}).available()
	for _, want := range []string{"setting up uid map: Permission denied", "kernel.apparmor_restrict_unprivileged_userns = 1", "docs/confinement-linux.md"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("with user namespaces refused: got %v, want a reason that holds %q", err, want)
		}
	}

	fakeBwrap(t, "0")
	err = (bubblewrap{}).available()
	if err == nil || !strings.Contains(err.Error(), "setting up uid map") || strings.Contains(err.Error(), "apparmor") {
		t.Errorf("with the restriction lifted: got %v, want bubblewrap's own reason alone", err)
	}
}

func TestBwrapMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := (bubblewrap{}).available(); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("available: got %v, want the sandbox missing", err)
	}
	if argv, err := (bubblewrap{}).render(resolved{}, []string{"/bin/cat"}); err == nil {
		t.Errorf("render: got %q, want the sandbox missing", argv)
	}
}

// requireLinux skips a test of what Command does through bubblewrap, which
// it runs on Linux alone, and fails it inside a sandbox.
func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("Command runs bubblewrap on Linux alone, and this is %s", runtime.GOOS)
	}
	if (bubblewrap{}).confined() {
		t.Fatal("this test runs inside a sandbox; leave it out with --exclude sandbox")
	}
}

func TestBwrapMissingRefusesCommand(t *testing.T) {
	requireLinux(t)
	t.Setenv("PATH", t.TempDir())
	cmd, err := Command(context.Background(), Profile{}, "/usr/bin/true")
	if !refusal(err) || cmd != nil {
		t.Errorf("got %v, want an error of kind refused and no command", err)
	}
}

func TestBwrapUsernsRefused(t *testing.T) {
	requireLinux(t)
	fakeBwrap(t, "1")
	const restriction = "kernel.apparmor_restrict_unprivileged_userns = 1"

	cmd, err := Command(context.Background(), Profile{}, "/usr/bin/true")
	if !refusal(err) || cmd != nil || !strings.Contains(err.Error(), restriction) {
		t.Errorf("Command: got %v, want an error of kind refused that names %q, and no command", err, restriction)
	}

	dir := t.TempDir()
	err = Verify(context.Background(), dir)
	if !refusal(err) || !strings.Contains(err.Error(), restriction) {
		t.Errorf("Verify: got %v, want an error of kind refused that names %q", err, restriction)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("Verify recorded %v in %s, want nothing: %v", entries, dir, err)
	}
}

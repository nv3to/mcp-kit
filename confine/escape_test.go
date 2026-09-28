package confine_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mcpkit "github.com/nv3to/mcp-kit"
	"github.com/nv3to/mcp-kit/confine"
)

// canaryText is what lies outside every profile. No confined command may
// print it.
const canaryText = "CANARY-mcpkit-confine-escape-suite"

// writeScript writes to the path that follows it on the command line.
const writeScript = `echo written > "$0"`

// holdScript prints the temporary directory and stays until it is killed.
const holdScript = `echo "$TMPDIR"; exec /bin/sleep 60`

type fixture struct {
	readOnly  string
	readWrite string
	outside   string
	allowed   string
	canary    string
	profile   confine.Profile
}

func resolve(t *testing.T, path string) string {
	t.Helper()
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return target
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func content(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func kindOf(err error) mcpkit.Kind {
	var e *mcpkit.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	requireBackend(t)
	root := resolve(t, t.TempDir())
	f := fixture{
		readOnly:  filepath.Join(root, "read-only"),
		readWrite: filepath.Join(root, "read-write"),
		outside:   filepath.Join(root, "outside"),
	}
	for _, dir := range []string{f.readOnly, f.readWrite, f.outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	f.allowed = filepath.Join(f.readOnly, "allowed.txt")
	f.canary = filepath.Join(f.outside, "canary.txt")
	write(t, f.allowed, "allowed")
	write(t, f.canary, canaryText)
	f.profile = confine.Profile{
		ReadOnly:  []string{f.readOnly},
		ReadWrite: []string{f.readWrite},
	}
	return f
}

type outcome struct {
	err    error
	output string
}

// try runs one confined command to its end. The canary in its output fails
// the test whatever the case is about, and so does a command that was killed.
func try(t *testing.T, profile confine.Profile, name string, args ...string) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd, err := confine.Command(ctx, profile, name, args...)
	if err != nil {
		t.Fatalf("Command %s %q: %v", name, args, err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	// A command that a signal ended did not get as far as the attempt, so
	// its failure is no evidence that the attempt was refused.
	var exit *exec.ExitError
	if errors.As(err, &exit) && !exit.Exited() {
		t.Fatalf("%s %q did not run to its end: %v: %q", name, args, err, out.String())
	}
	if strings.Contains(out.String(), canaryText) {
		t.Errorf("%s %q printed the canary: %q", name, args, out.String())
	}
	return outcome{err: err, output: out.String()}
}

// hold starts a confined command that stays, and returns its temporary
// directory. stop kills it through the context and waits for it.
func hold(t *testing.T, profile confine.Profile) (tmp string, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := confine.Command(ctx, profile, "/bin/sh", "-c", holdScript)
	if err != nil {
		cancel()
		t.Fatalf("Command: %v", err)
	}
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	var once sync.Once
	var waited error
	stop = func() error {
		once.Do(func() {
			cancel()
			waited = cmd.Wait()
			writer.Close()
		})
		return waited
	}
	t.Cleanup(func() { stop() })

	line := make(chan string, 1)
	go func() {
		first, _ := bufio.NewReader(reader).ReadString('\n')
		line <- strings.TrimSpace(first)
		io.Copy(io.Discard, reader)
	}()
	select {
	case tmp = <-line:
	case <-time.After(20 * time.Second):
		t.Fatalf("the command did not print its temporary directory: %v", stop())
	}
	if !filepath.IsAbs(tmp) {
		t.Fatalf("the temporary directory is %q: %v", tmp, stop())
	}
	return tmp, stop
}

// accountHome is the home directory of the account. A test runner may point
// HOME somewhere else, so the shell looks it up.
func accountHome(t *testing.T) string {
	t.Helper()
	name, err := exec.Command("/usr/bin/id", "-un").Output()
	if err != nil {
		t.Fatalf("look up the account name: %v", err)
	}
	out, err := exec.Command("/bin/sh", "-c", "echo ~"+strings.TrimSpace(string(name))).Output()
	if err != nil {
		t.Fatalf("expand the home directory: %v", err)
	}
	home := strings.TrimSpace(string(out))
	if !filepath.IsAbs(home) {
		t.Fatalf("the home directory is %q", home)
	}
	return resolve(t, home)
}

// proxyAddr stands for the address of a proxy where none is dialled: confine
// takes an address and knows nothing of what listens there.
const proxyAddr = "127.0.0.1:3128"

// eachNetwork runs a case under a closed network and under a proxy, because
// what a profile says of the network must not change what it says of files
// and of the environment.
func eachNetwork(t *testing.T, run func(t *testing.T, f fixture)) {
	t.Helper()
	for _, network := range []struct {
		name string
		is   confine.Network
	}{
		{"closed", confine.None},
		{"proxy", confine.Proxy(proxyAddr)},
	} {
		t.Run(network.name, func(t *testing.T) {
			f := newFixture(t)
			f.profile.Network = network.is
			run(t, f)
		})
	}
}

func TestEscapeReadOutsideTheProfile(t *testing.T) {
	eachNetwork(t, escapeReadOutsideTheProfile)
}

func escapeReadOutsideTheProfile(t *testing.T, f fixture) {
	if r := try(t, f.profile, "/bin/cat", f.canary); r.err == nil {
		t.Errorf("read of %s: want it to fail, got %q", f.canary, r.output)
	}
}

func TestEscapeReadUnderTheHomeDirectory(t *testing.T) {
	eachNetwork(t, escapeReadUnderTheHomeDirectory)
}

func escapeReadUnderTheHomeDirectory(t *testing.T, f fixture) {
	dir, err := os.MkdirTemp(accountHome(t), ".mcpkit-confine-escape-")
	if err != nil {
		t.Fatalf("create the fixture under the home directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	canary := filepath.Join(dir, "canary.txt")
	write(t, canary, canaryText)

	if r := try(t, f.profile, "/bin/cat", canary); r.err == nil {
		t.Errorf("read of %s: want it to fail, got %q", canary, r.output)
	}
	if r := try(t, f.profile, "/bin/ls", dir); r.err == nil {
		t.Errorf("listing of %s: want it to fail, got %q", dir, r.output)
	}
}

func TestEscapeWriteOutsideTheWritablePaths(t *testing.T) {
	eachNetwork(t, escapeWriteOutsideTheWritablePaths)
}

func escapeWriteOutsideTheWritablePaths(t *testing.T, f fixture) {
	target := filepath.Join(f.outside, "written.txt")
	r := try(t, f.profile, "/bin/sh", "-c", writeScript, target)
	if r.err == nil || exists(target) {
		t.Errorf("write of %s: want it to fail, got %v and %q", target, r.err, r.output)
	}
}

func TestEscapeWriteToAReadOnlyPath(t *testing.T) {
	eachNetwork(t, escapeWriteToAReadOnlyPath)
}

func escapeWriteToAReadOnlyPath(t *testing.T, f fixture) {
	target := filepath.Join(f.readOnly, "written.txt")
	r := try(t, f.profile, "/bin/sh", "-c", writeScript, target)
	if r.err == nil || exists(target) {
		t.Errorf("write of %s: want it to fail, got %v and %q", target, r.err, r.output)
	}
	r = try(t, f.profile, "/bin/sh", "-c", writeScript, f.allowed)
	if got := content(t, f.allowed); r.err == nil || got != "allowed" {
		t.Errorf("write over %s: want it to fail, got %v and the content %q", f.allowed, r.err, got)
	}
}

func TestEscapeWriteThroughASymlink(t *testing.T) {
	eachNetwork(t, escapeWriteThroughASymlink)
}

func escapeWriteThroughASymlink(t *testing.T, f fixture) {
	target := filepath.Join(f.outside, "target.txt")
	write(t, target, "untouched")
	toFile := filepath.Join(f.readWrite, "to-file")
	toDir := filepath.Join(f.readWrite, "to-dir")
	if err := os.Symlink(target, toFile); err != nil {
		t.Fatalf("link %s: %v", toFile, err)
	}
	if err := os.Symlink(f.outside, toDir); err != nil {
		t.Fatalf("link %s: %v", toDir, err)
	}

	r := try(t, f.profile, "/bin/sh", "-c", writeScript, toFile)
	if got := content(t, target); r.err == nil || got != "untouched" {
		t.Errorf("write through %s: want it to fail, got %v and the content %q", toFile, r.err, got)
	}
	created := filepath.Join(f.outside, "created.txt")
	r = try(t, f.profile, "/bin/sh", "-c", writeScript, filepath.Join(toDir, "created.txt"))
	if r.err == nil || exists(created) {
		t.Errorf("write through %s: want it to fail, got %v and %q", toDir, r.err, r.output)
	}
	if r := try(t, f.profile, "/bin/cat", filepath.Join(toDir, "canary.txt")); r.err == nil {
		t.Errorf("read through %s: want it to fail, got %q", toDir, r.output)
	}
}

// proxyNames are the variables a proxy adds to the environment.
var proxyNames = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

func TestEscapeEnvironment(t *testing.T) {
	eachNetwork(t, escapeEnvironment)
}

func escapeEnvironment(t *testing.T, f fixture) {
	t.Setenv("CONFINE_TEST_SECRET", canaryText)
	// The server's own proxy is not the command's.
	t.Setenv("HTTPS_PROXY", "http://192.0.2.1:8080")
	t.Setenv("no_proxy", "*")
	t.Setenv("CONFINE_TEST_PASSED", "passed")
	t.Setenv("CONFINE_TEST_REPLACED", "from the server")
	os.Unsetenv("CONFINE_TEST_UNSET")
	f.profile.Env.Pass = []string{"CONFINE_TEST_PASSED", "CONFINE_TEST_REPLACED", "CONFINE_TEST_UNSET"}
	f.profile.Env.Set = map[string]string{
		"CONFINE_TEST_FIXED":    "fixed",
		"CONFINE_TEST_REPLACED": "from the profile",
	}

	r := try(t, f.profile, "/usr/bin/env")
	if r.err != nil {
		t.Fatalf("env: %v: %q", r.err, r.output)
	}
	got := map[string]string{}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(r.output), "\n") {
		name, value, _ := strings.Cut(line, "=")
		got[name] = value
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"CONFINE_TEST_FIXED", "CONFINE_TEST_PASSED", "CONFINE_TEST_REPLACED", confine.Marker, confine.TempVar}
	if f.profile.Network != confine.None {
		want = append(want, proxyNames...)
		for _, name := range proxyNames {
			value := "http://" + proxyAddr
			if strings.EqualFold(name, "no_proxy") {
				value = ""
			}
			if got[name] != value {
				t.Errorf("%s is %q, want %q", name, got[name], value)
			}
		}
	}
	sort.Strings(want)
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("the environment has the names %q, want exactly %q", names, want)
	}
	if got["CONFINE_TEST_PASSED"] != "passed" || got["CONFINE_TEST_FIXED"] != "fixed" || got["CONFINE_TEST_REPLACED"] != "from the profile" {
		t.Errorf("the environment has the values %q", got)
	}
	if !filepath.IsAbs(got[confine.TempVar]) {
		t.Errorf("%s is %q, want a directory", confine.TempVar, got[confine.TempVar])
	}
}

func TestProxyVariablesAreNotTheProfiles(t *testing.T) {
	eachNetwork(t, func(t *testing.T, f fixture) {
		for _, name := range proxyNames {
			t.Setenv(name, "http://192.0.2.1:8080")
			for _, env := range []confine.Env{
				{Pass: []string{name}},
				{Set: map[string]string{name: "http://192.0.2.1:8080"}},
			} {
				f.profile.Env = env
				cmd, err := confine.Command(context.Background(), f.profile, "/usr/bin/env")
				if kindOf(err) != mcpkit.Invalid || cmd != nil {
					t.Errorf("environment %+v: got %v, want an error of kind invalid and no command", env, err)
				}
			}
		}
	})
}

func TestProxyThatIsNoLoopbackPort(t *testing.T) {
	f := newFixture(t)
	for _, addr := range []string{"", "127.0.0.1", "localhost:3128", "127.0.0.1:0", "192.0.2.1:3128", "0.0.0.0:3128", "[::]:3128", "http://127.0.0.1:3128"} {
		f.profile.Network = confine.Proxy(addr)
		cmd, err := confine.Command(context.Background(), f.profile, "/usr/bin/true")
		if kindOf(err) != mcpkit.Invalid || cmd != nil {
			t.Errorf("proxy %q: got %v, want an error of kind invalid and no command", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:3128", "[::1]:3128"} {
		f.profile.Network = confine.Proxy(addr)
		if _, err := confine.Command(context.Background(), f.profile, "/usr/bin/true"); err != nil {
			t.Errorf("proxy %q: got %v, want a command", addr, err)
		}
	}
}

func TestEscapeTemporaryDirectoryOfAnotherCommand(t *testing.T) {
	eachNetwork(t, escapeTemporaryDirectoryOfAnotherCommand)
}

func escapeTemporaryDirectoryOfAnotherCommand(t *testing.T, f fixture) {
	tmp, stop := hold(t, f.profile)
	canary := filepath.Join(tmp, "canary.txt")
	write(t, canary, canaryText)

	if r := try(t, f.profile, "/bin/cat", canary); r.err == nil {
		t.Errorf("read of %s: want it to fail, got %q", canary, r.output)
	}
	if r := try(t, f.profile, "/bin/ls", tmp); r.err == nil {
		t.Errorf("listing of %s: want it to fail, got %q", tmp, r.output)
	}
	target := filepath.Join(tmp, "written.txt")
	r := try(t, f.profile, "/bin/sh", "-c", writeScript, target)
	if r.err == nil || exists(target) {
		t.Errorf("write of %s: want it to fail, got %v and %q", target, r.err, r.output)
	}
	stop()
}

func TestAllowedReadAndWrite(t *testing.T) {
	eachNetwork(t, allowedReadAndWrite)
}

func allowedReadAndWrite(t *testing.T, f fixture) {
	if r := try(t, f.profile, "/bin/cat", f.allowed); r.err != nil || r.output != "allowed" {
		t.Errorf("read of %s: got %v and %q, want it to succeed", f.allowed, r.err, r.output)
	}
	target := filepath.Join(f.readWrite, "written.txt")
	r := try(t, f.profile, "/bin/sh", "-c", writeScript, target)
	if r.err != nil || !exists(target) || content(t, target) != "written\n" {
		t.Errorf("write of %s: got %v and %q, want it to succeed", target, r.err, r.output)
	}
	r = try(t, f.profile, "/bin/sh", "-c", `echo kept > "$TMPDIR/file" && /bin/cat "$TMPDIR/file"`)
	if r.err != nil || r.output != "kept\n" {
		t.Errorf("write in the temporary directory: got %v and %q, want it to succeed", r.err, r.output)
	}
}

func TestWorkingDirectory(t *testing.T) {
	eachNetwork(t, workingDirectory)
}

func workingDirectory(t *testing.T, f fixture) {
	ctx := context.Background()

	cmd, err := confine.Command(ctx, f.profile, "/bin/cat", "allowed.txt")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Dir = f.readOnly
	if err := cmd.Run(); err != nil || out.String() != "allowed" {
		t.Errorf("in %s: got %v and %q, want the file read by its relative name", f.readOnly, err, out.String())
	}

	cmd, err = confine.Command(ctx, f.profile, "/bin/cat", "canary.txt")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	cmd.Dir = f.outside
	if err := cmd.Run(); kindOf(err) != mcpkit.Invalid {
		t.Errorf("in %s: got %v, want an error of kind invalid", f.outside, err)
	}
}

func TestTemporaryDirectoryIsRemoved(t *testing.T) {
	f := newFixture(t)

	r := try(t, f.profile, "/bin/sh", "-c", `echo "$TMPDIR"`)
	tmp := strings.TrimSpace(r.output)
	if r.err != nil || !filepath.IsAbs(tmp) {
		t.Fatalf("print the temporary directory: %v: %q", r.err, r.output)
	}
	if exists(tmp) {
		t.Errorf("%s is still there after the command ended", tmp)
	}

	tmp, stop := hold(t, f.profile)
	if !exists(tmp) {
		t.Errorf("%s is not there while the command runs", tmp)
	}
	if err := stop(); err == nil {
		t.Errorf("the command was killed through the context: want an error from Wait")
	}
	if exists(tmp) {
		t.Errorf("%s is still there after the command was killed", tmp)
	}
}

func TestPathThatDoesNotExist(t *testing.T) {
	f := newFixture(t)
	missing := filepath.Join(f.outside, "missing")
	for _, profile := range []confine.Profile{
		{ReadOnly: []string{missing}},
		{ReadWrite: []string{missing}},
	} {
		cmd, err := confine.Command(context.Background(), profile, "/bin/cat", f.allowed)
		if kindOf(err) != mcpkit.NotFound || cmd != nil {
			t.Errorf("profile %+v: got %v, want an error of kind not_found and no command", profile, err)
		}
	}
}

func TestVerdict(t *testing.T) {
	f := newFixture(t)
	parent := filepath.Join(f.outside, "state")
	dir := filepath.Join(parent, "verdict")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	if ok, reason := confine.Verified(dir); ok || reason == "" {
		t.Errorf("before Verify: got %v and %q, want false and a reason", ok, reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := confine.Verify(ctx, dir); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("Verify recorded nothing in %s: %v", dir, err)
	}
	if ok, reason := confine.Verified(dir); !ok {
		t.Errorf("after Verify: got false and %q, want true", reason)
	}

	for _, writable := range []string{dir, parent} {
		profile := confine.Profile{ReadWrite: []string{writable}}
		cmd, err := confine.Command(ctx, profile, "/bin/cat", f.allowed)
		if kindOf(err) != mcpkit.Refused || cmd != nil {
			t.Errorf("writable %s: got %v, want an error of kind refused and no command", writable, err)
		}
	}
}

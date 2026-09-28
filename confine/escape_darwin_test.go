//go:build darwin

package confine_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// dataVolume is where macOS keeps the files of users. Every path below it is
// a second name of a path at the root.
const dataVolume = "/System/Volumes/Data"

func TestEscapeReadThroughTheDataVolume(t *testing.T) {
	eachNetwork(t, escapeReadThroughTheDataVolume)
}

func escapeReadThroughTheDataVolume(t *testing.T, f fixture) {
	dir, err := os.MkdirTemp(accountHome(t), ".mcpkit-confine-escape-")
	if err != nil {
		t.Fatalf("create the fixture under the home directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	home := filepath.Join(dir, "canary.txt")
	write(t, home, canaryText)

	for _, canary := range []string{home, f.canary} {
		second := filepath.Join(dataVolume, canary)
		if got := content(t, second); got != canaryText {
			t.Fatalf("%s is no second name of %s: %q", second, canary, got)
		}
		if r := try(t, f.profile, "/bin/cat", second); r.err == nil {
			t.Errorf("read of %s: want it to fail, got %q", second, r.output)
		}
	}
	target := filepath.Join(dataVolume, f.readOnly, "written.txt")
	r := try(t, f.profile, "/bin/sh", "-c", writeScript, target)
	if r.err == nil || exists(filepath.Join(f.readOnly, "written.txt")) {
		t.Errorf("write of %s: want it to fail, got %v and %q", target, r.err, r.output)
	}
}

// running reports whether a process of that name runs.
func running(name string) bool {
	return exec.Command("/usr/bin/pgrep", "-x", name).Run() == nil
}

func TestEscapeStartAnApplication(t *testing.T) {
	eachNetwork(t, escapeStartAnApplication)
}

// An application that /usr/bin/open starts is started by the system and
// runs outside every sandbox.
func escapeStartAnApplication(t *testing.T, f fixture) {
	const application = "Calculator"
	if running(application) {
		t.Skipf("%s runs already, so the attempt would prove nothing", application)
	}
	t.Cleanup(func() {
		if running(application) {
			exec.Command("/usr/bin/pkill", "-x", application).Run()
		}
	})
	r := try(t, f.profile, "/usr/bin/open", "-g", "-a", application)
	started := false
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end) && !started; time.Sleep(100 * time.Millisecond) {
		started = running(application)
	}
	if r.err == nil || started {
		t.Errorf("/usr/bin/open -a %s: want it to fail and start nothing, got %v, %q and started=%v", application, r.err, r.output, started)
	}
}

func TestEscapeAskTheServiceManager(t *testing.T) {
	eachNetwork(t, escapeAskTheServiceManager)
}

// launchd starts what it is asked to outside every sandbox, so a confined
// command must not reach it.
func escapeAskTheServiceManager(t *testing.T, f fixture) {
	r := try(t, f.profile, "/bin/launchctl", "list")
	if r.err == nil || strings.Contains(r.output, "com.apple.") {
		t.Errorf("/bin/launchctl list: want it to fail, got %v and %q", r.err, r.output)
	}
}

// childEnv makes TestProcessTable ask the kernel for the table of processes,
// so that the test binary can be the confined command.
const childEnv = "MCPKIT_ESCAPE_PROCESSES"

// childRun selects that test alone.
const childRun = "-test.run=^TestProcessTable$"

func TestProcessTable(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("the confined command of TestEscapeListTheProcesses")
	}
	// kern.proc.all, asked for its size alone.
	mib := [4]int32{1, 14, 0, 0}
	var size uintptr
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])), 3, 0, uintptr(unsafe.Pointer(&size)), 0, 0)
	if errno != 0 || size == 0 {
		t.Fatalf("refused: %v", errno)
	}
	fmt.Println("processes:", size)
}

func TestEscapeListTheProcesses(t *testing.T) {
	eachNetwork(t, escapeListTheProcesses)
}

// The table of processes names every program of the host and its owner.
func escapeListTheProcesses(t *testing.T, f fixture) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	self = resolve(t, self)
	outside := exec.Command(self, childRun)
	outside.Env = []string{childEnv + "=1"}
	if out, err := outside.CombinedOutput(); err != nil {
		t.Fatalf("the table cannot be read outside the sandbox, so the attempt proves nothing: %v: %q", err, out)
	}
	f.profile.ReadOnly = append(f.profile.ReadOnly, filepath.Dir(self))
	f.profile.Env.Set = map[string]string{childEnv: "1"}
	if r := try(t, f.profile, self, childRun); r.err == nil || !strings.Contains(r.output, "refused") {
		t.Errorf("read of the table of processes: want it refused, got %v and %q", r.err, r.output)
	}
}

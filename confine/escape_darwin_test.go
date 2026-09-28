//go:build darwin

package confine_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

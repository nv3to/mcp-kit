//go:build linux

package confine_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestEscapeListTheProcesses(t *testing.T) {
	eachNetwork(t, escapeListTheProcesses)
}

// The table of processes names every program of the host and its owner. A
// confined command has a /proc of its own, which lists its own processes.
func escapeListTheProcesses(t *testing.T, f fixture) {
	self := strconv.Itoa(os.Getpid())
	if !exists(filepath.Join("/proc", self)) {
		t.Fatalf("/proc lists no process %s outside the sandbox, so the attempt proves nothing", self)
	}
	r := try(t, f.profile, "/bin/ls", "/proc")
	if r.err != nil {
		t.Fatalf("listing of /proc: %v: %q", r.err, r.output)
	}
	for _, name := range strings.Fields(r.output) {
		if name == self {
			t.Errorf("the listing of /proc names the process %s of the host: %q", self, r.output)
		}
	}
}

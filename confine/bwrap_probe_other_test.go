//go:build !linux

package confine

import (
	"runtime"
	"testing"
)

// The bubblewrap probes exist on Linux only. Here they are reported as
// skipped, so a run on another system does not read as evidence.
func TestBwrapProbes(t *testing.T) {
	t.Skipf("bubblewrap is a Linux sandbox and this is %s; the run of .github/workflows/linux.yml is the evidence", runtime.GOOS)
}

//go:build !darwin && !linux

package nettest_test

import (
	"runtime"
	"testing"
)

// requireBackend skips the test where confine has no sandbox and starts no
// command, so a run there does not read as evidence.
func requireBackend(t *testing.T) {
	t.Helper()
	t.Skipf("confine has no sandbox on %s", runtime.GOOS)
}

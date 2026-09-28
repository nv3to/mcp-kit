//go:build !darwin && !linux

package confine_test

import (
	"runtime"
	"testing"
)

// requireBackend skips the test where confine has no sandbox, so a run
// there does not read as evidence.
func requireBackend(t *testing.T) {
	t.Helper()
	t.Skipf("confine has no sandbox for %s", runtime.GOOS)
}

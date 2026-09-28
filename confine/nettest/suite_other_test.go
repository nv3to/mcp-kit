//go:build !darwin

package nettest_test

import (
	"runtime"
	"testing"
)

// requireBackend skips the test where a confined command cannot reach the
// proxy, so a run there does not read as evidence.
func requireBackend(t *testing.T) {
	t.Helper()
	t.Skipf("a command that confine starts on %s cannot reach a proxy", runtime.GOOS)
}

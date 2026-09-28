//go:build darwin

package confine_test

import (
	"testing"

	"github.com/nv3to/mcp-kit/confine"
)

// requireBackend fails the test when no sandbox can be started, which is
// what happens when the test process is itself under a profile.
func requireBackend(t *testing.T) {
	t.Helper()
	if confine.Confined() {
		t.Fatal("this test runs inside a sandbox and cannot start one; leave it out with --exclude sandbox")
	}
}

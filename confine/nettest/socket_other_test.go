//go:build !linux

package nettest_test

import (
	"runtime"
	"testing"
)

// skipSocket skips a case of the socket through which a command on Linux
// reaches the proxy.
func skipSocket(t *testing.T) {
	t.Helper()
	t.Skipf("a command on %s reaches the proxy without a socket; the socket exists on Linux alone", runtime.GOOS)
}

func TestEscapeThroughTheSocket(t *testing.T) { skipSocket(t) }

func TestForwarderOfEachCommand(t *testing.T) { skipSocket(t) }

func TestNothingIsLeftListening(t *testing.T) { skipSocket(t) }

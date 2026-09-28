//go:build linux

package confine_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nv3to/mcp-kit/confine"
)

// certificates is where Debian and Ubuntu keep the certificates they trust.
const certificates = "/etc/ssl/certs/ca-certificates.crt"

func TestCertificatesWithAProxyAlone(t *testing.T) {
	if !exists(certificates) {
		t.Skipf("%s does not exist on this system", certificates)
	}
	eachNetwork(t, certificatesWithAProxyAlone)
}

// A command that may reach the proxy reads the certificates, to verify the
// host behind a tunnel; with the network closed it has no use for them.
func certificatesWithAProxyAlone(t *testing.T, f fixture) {
	r := try(t, f.profile, "/bin/cat", certificates)
	if f.profile.Network == confine.None {
		if r.err == nil {
			t.Errorf("read of %s with the network closed: want it to fail", certificates)
		}
		return
	}
	if r.err != nil || !strings.Contains(r.output, "BEGIN CERTIFICATE") {
		t.Errorf("read of %s with a proxy: got %v, want the certificates", certificates, r.err)
	}
}

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

//go:build darwin

package confine

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpkit "github.com/nv3to/mcp-kit"
)

func refusal(err error) bool {
	var e *mcpkit.Error
	return errors.As(err, &e) && e.Kind == mcpkit.Refused
}

func TestSeatbeltMissing(t *testing.T) {
	found := seatbeltPath
	seatbeltPath = filepath.Join(t.TempDir(), "sandbox-exec")
	t.Cleanup(func() { seatbeltPath = found })

	cmd, err := Command(context.Background(), Profile{}, "/usr/bin/true")
	if !refusal(err) {
		t.Errorf("got %v, want an error of kind refused", err)
	}
	if cmd != nil {
		t.Errorf("got a command that could be started, want none")
	}
}

func mustRender(t *testing.T, r resolved) string {
	t.Helper()
	profile, err := seatbeltProfile(r)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return profile
}

func TestSeatbeltRender(t *testing.T) {
	r := resolved{
		readOnly:  []string{"/opt/read \"only\""},
		readWrite: []string{"/opt/read-write"},
		tmp:       "/private/tmp/confine-1",
	}
	closed := strings.Index(mustRender(t, r), "(deny file-read* (subpath \"/opt\"))\n")
	opened := strings.Index(mustRender(t, r), "(allow file-read* (subpath \"/opt/read-write\"))\n")
	if closed < 0 || opened < closed {
		t.Errorf("the denial of /opt must come before the allowed path, which overrides it: %d, %d", closed, opened)
	}
	first, err := seatbeltProfile(r)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := seatbeltProfile(r)
	if err != nil || first != second {
		t.Errorf("render is not a function of the profile:\n%s\n%s", first, second)
	}
	for _, rule := range []string{
		"(deny network*)\n",
		"(deny file-write*)\n",
		"(deny file-read* (subpath \"/Users\"))\n",
		"(deny file-read* (subpath \"/private/var/folders\"))\n",
		"(allow file-read-metadata (literal \"/opt\"))\n",
		"(allow file-read* (subpath \"/opt/read \\\"only\\\"\"))\n",
		"(allow file-read* (subpath \"/opt/read-write\"))\n",
		"(allow file-write* (subpath \"/opt/read-write\"))\n",
		"(allow file-write* (subpath \"/private/tmp/confine-1\"))\n",
	} {
		if !strings.Contains(first, rule) {
			t.Errorf("the profile lacks %q:\n%s", rule, first)
		}
	}
	if strings.Contains(first, "(allow file-write* (subpath \"/opt/read \\\"only\\\"\"))") {
		t.Errorf("the profile lets the read-only path be written:\n%s", first)
	}
}

func TestSeatbeltRenderProxy(t *testing.T) {
	const rule = "(allow network-outbound (remote ip \"localhost:3128\"))\n"
	closed := mustRender(t, resolved{})
	if strings.Contains(closed, "network-outbound") {
		t.Errorf("the profile of a closed network allows traffic:\n%s", closed)
	}
	narrowed := mustRender(t, resolved{proxy: netip.MustParseAddrPort("127.0.0.1:3128")})
	denied := strings.Index(narrowed, "(deny network*)\n")
	allowed := strings.Index(narrowed, rule)
	if denied < 0 || allowed < denied {
		t.Errorf("the denial of the network must come before the proxy, which overrides it: %d, %d:\n%s", denied, allowed, narrowed)
	}
	if got := strings.Count(narrowed, "(allow network"); got != 1 {
		t.Errorf("the profile allows %d network operations, want the proxy alone:\n%s", got, narrowed)
	}
	if _, err := seatbeltProfile(resolved{proxy: netip.MustParseAddrPort("192.0.2.1:3128")}); err == nil {
		t.Errorf("a proxy that is not on a loopback address: want it refused")
	}
}

func TestSeatbeltCannotExpress(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "two\nlines")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create %q: %v", dir, err)
	}
	if (seatbelt{}).confined() {
		t.Fatal("this test runs inside a sandbox; leave it out with --exclude sandbox")
	}
	cmd, err := Command(context.Background(), Profile{ReadOnly: []string{dir}}, "/usr/bin/true")
	if !refusal(err) || cmd != nil {
		t.Errorf("got %v, want an error of kind refused and no command", err)
	}
}

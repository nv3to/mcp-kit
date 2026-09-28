//go:build linux

package nettest_test

// On Linux the command reaches the proxy through a unix socket in its view:
// the forwarder inside the sandbox carries every connection there, and a
// relay outside carries it on to the proxy.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nv3to/mcp-kit/confine"
	"github.com/nv3to/mcp-kit/egress"
)

// goOn is the file the test puts into the temporary directory of a held
// command to let it go on.
const goOn = "proceed"

func init() {
	attempts["socket"] = throughTheSocket
	attempts["replace"] = replaceTheSocket
	attempts["connect"] = connect
	attempts["hold"] = holdThenGet
}

// sockets returns the unix sockets the command sees: those in the
// directories that are mounted into its sandbox.
func sockets() ([]string, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	seen := map[string]bool{}
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		point := unescape.Replace(fields[4])
		entries, err := os.ReadDir(point)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			path := filepath.Join(point, entry.Name())
			if entry.Type()&fs.ModeSocket != 0 && !seen[path] {
				seen[path] = true
				found = append(found, path)
			}
		}
	}
	return found, nil
}

// theSocket returns the one unix socket the command sees. Any other number
// means that the attempt cannot be made.
func theSocket() (string, bool) {
	found, err := sockets()
	if err != nil || len(found) != 1 {
		fmt.Printf("the command sees the unix sockets %q, want one: %v\n", found, err)
		return "", false
	}
	return found[0], true
}

// throughTheSocket sends a request for a host straight to the socket, past
// the forwarder, and prints the answer.
func throughTheSocket(args []string) int {
	socket, ok := theSocket()
	if !ok || len(args) != 1 {
		return childBroken
	}
	conn, err := net.DialTimeout("unix", socket, attemptDeadline)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		return childFailed
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(attemptDeadline))
	fmt.Fprintf(conn, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", args[0], args[0])
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		return childFailed
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		return childFailed
	}
	fmt.Printf("status %d: %s\n", resp.StatusCode, strings.TrimSpace(string(answer)))
	if resp.StatusCode != http.StatusOK {
		return childFailed
	}
	return childDone
}

// replaceTheSocket tries to take the socket from the forwarder: to remove or
// move it or its directory, and to put a socket or a file of its own beside
// it. It is done when any of these succeeds.
func replaceTheSocket([]string) int {
	socket, ok := theSocket()
	if !ok {
		return childBroken
	}
	dir := filepath.Dir(socket)
	status := childFailed
	for _, try := range []struct {
		what string
		do   func() error
	}{
		{"remove the socket", func() error { return os.Remove(socket) }},
		{"move the socket", func() error { return os.Rename(socket, filepath.Join(dir, "moved")) }},
		{"move its directory", func() error { return os.Rename(dir, dir+".moved") }},
		{"listen beside it", func() error {
			l, err := net.Listen("unix", filepath.Join(dir, "other"))
			if err == nil {
				l.Close()
			}
			return err
		}},
		{"write beside it", func() error { return os.WriteFile(filepath.Join(dir, "file"), nil, 0o600) }},
	} {
		if err := try.do(); err != nil {
			fmt.Printf("%s: failed: %v\n", try.what, err)
			continue
		}
		fmt.Printf("%s: done\n", try.what)
		status = childDone
	}
	return status
}

// connect connects to a unix socket.
func connect(args []string) int {
	if len(args) != 1 {
		return childBroken
	}
	conn, err := net.DialTimeout("unix", args[0], attemptDeadline)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		return childFailed
	}
	conn.Close()
	fmt.Println("done")
	return childDone
}

// holdThenGet prints its socket and its temporary directory, waits until the
// test puts goOn there, and then downloads through the proxy.
func holdThenGet(args []string) int {
	socket, ok := theSocket()
	if !ok || len(args) != 1 {
		return childBroken
	}
	tmp := os.Getenv(confine.TempVar)
	fmt.Println(socket)
	fmt.Println(tmp)
	for end := time.Now().Add(time.Minute); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(tmp, goOn)); err == nil {
			break
		}
		if time.Now().After(end) {
			fmt.Println("the test did not let the command go on")
			return childBroken
		}
	}
	return request(http.MethodGet, args[0], "")
}

// holding is a confined command that holds its forwarder until the test
// lets it go on.
type holding struct {
	socket string
	tmp    string
	stop   context.CancelFunc
	ended  chan error
	output chan string
}

func (w *world) hold(t *testing.T, url string) holding {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	profile := w.profile
	profile.Env.Set = map[string]string{childEnv: "hold"}
	cmd, err := confine.Command(ctx, profile, w.self, url)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ended := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		writer.Close()
		ended <- err
	}()
	lines := bufio.NewReader(reader)
	var first []string
	for len(first) < 2 {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("the held command printed %q after %q: %v", line, first, err)
		}
		first = append(first, strings.TrimSpace(line))
	}
	output := make(chan string, 1)
	go func() {
		rest, _ := io.ReadAll(lines)
		output <- string(rest)
	}()
	return holding{socket: first[0], tmp: first[1], stop: cancel, ended: ended, output: output}
}

func (h holding) proceed(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.tmp, goOn), nil, 0o600); err != nil {
		t.Fatalf("let the held command go on: %v", err)
	}
}

func (h holding) wait() (output string, err error) {
	err = <-h.ended
	return <-h.output, err
}

// forwarding reports whether a process of the host runs with that socket
// among its arguments: the forwarder, or the bubblewrap that started it.
func forwarding(t *testing.T, socket string) bool {
	t.Helper()
	cmdlines, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		t.Fatalf("list the processes: %v", err)
	}
	for _, path := range cmdlines {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "\x00"+socket+"\x00") {
			return true
		}
	}
	return false
}

// forwardingEnds reports whether the processes of forwarding end within a
// few seconds. A killed sandbox ends its processes after Wait returns.
func forwardingEnds(t *testing.T, socket string) bool {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); forwarding(t, socket); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			return false
		}
	}
	return true
}

// Whatever reaches the socket reaches the proxy and is judged there, and the
// command can neither remove nor replace the socket.
func TestEscapeThroughTheSocket(t *testing.T) {
	w := newWorld(t)
	wantForbidden(t, w.attempt(t, w.profile, "socket", unlistedHost), egress.HostNotAllowed)
	if r := w.attempt(t, w.profile, "replace"); r.status != childFailed {
		t.Errorf("replace the socket: want every attempt to fail, got %q", r.output)
	}
	w.wantRefusals(t, egress.Refusal{Host: unlistedHost, Method: http.MethodGet, Reason: egress.HostNotAllowed})
}

// Two commands run at once, each with a forwarder of its own on the same
// address. Neither sees the socket of the other, and what one tries on its
// own socket leaves the other as it was.
func TestForwarderOfEachCommand(t *testing.T) {
	w := newWorld(t)
	url := "http://" + allowedHost + "/file"
	want := fmt.Sprintf("status %d: %s\n", http.StatusOK, downloaded)
	first := w.hold(t, url)

	if r := w.attempt(t, w.profile, "connect", first.socket); r.status != childFailed {
		t.Errorf("connect to the socket %s of another command: want it to fail, got %q", first.socket, r.output)
	}
	if r := w.attempt(t, w.profile, "replace"); r.status != childFailed {
		t.Errorf("replace the socket: want every attempt to fail, got %q", r.output)
	}
	if r := w.get(t, url); r.status != childDone || r.output != want {
		t.Errorf("download beside another command: got status %d and %q, want %q", r.status, r.output, want)
	}

	first.proceed(t)
	if output, err := first.wait(); err != nil || output != want {
		t.Errorf("download of the held command after the other one: got %v and %q, want %q", err, output, want)
	}
}

// When the command ends or is killed, its forwarder ends too, and the socket
// the forwarder reached is gone.
func TestNothingIsLeftListening(t *testing.T) {
	w := newWorld(t)
	for _, killed := range []bool{false, true} {
		h := w.hold(t, "http://"+allowedHost+"/file")
		if !forwarding(t, h.socket) {
			t.Fatalf("no process of the host runs the forwarder of %s, so its end proves nothing", h.socket)
		}
		if killed {
			h.stop()
		} else {
			h.proceed(t)
		}
		output, err := h.wait()
		var exit *exec.ExitError
		if killed != errors.As(err, &exit) {
			t.Errorf("killed %v: the command ended with %v and %q", killed, err, output)
		}
		if !forwardingEnds(t, h.socket) {
			t.Errorf("killed %v: the forwarder of %s runs after the command ended", killed, h.socket)
		}
		if _, err := os.Lstat(filepath.Dir(h.socket)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("killed %v: the directory of %s: got %v, want it removed", killed, h.socket, err)
		}
		if conn, err := net.Dial("unix", h.socket); err == nil {
			conn.Close()
			t.Errorf("killed %v: %s is still listening", killed, h.socket)
		}
	}
}

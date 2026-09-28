// Package nettest_test holds a confined command against a real egress proxy.
// It is a package of its own so that confine and egress need not know each
// other.
package nettest_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nv3to/mcp-kit/confine"
	"github.com/nv3to/mcp-kit/egress"
)

// childEnv names the attempt the test binary makes instead of the tests. The
// binary is its own confined command, so the suite needs no tool of the host
// and an attempt reports its error first hand.
const childEnv = "MCPKIT_NETTEST_ATTEMPT"

const (
	// allowedHost is on the allowlist and resolves to allowedAddr, which
	// the dialer of the test turns into the local servers.
	allowedHost = "downloads.test"
	allowedAddr = "203.0.113.10"
	// unlistedHost is on no allowlist.
	unlistedHost = "elsewhere.test"
	// privateHost is on the allowlist and resolves to privateAddr, which is
	// on the allowlist too.
	privateHost = "internal.test"
	privateAddr = "10.0.0.7"

	publicAddr = "1.1.1.1:443"
	publicName = "example.com"

	downloaded = "the content of the download"
	payload    = "what the command must not send"

	attemptDeadline = 5 * time.Second
)

// The child ends with childDone when the attempt succeeded and with
// childFailed when it was made and failed. Any other status says that the
// attempt was not made, which is no evidence of a refusal.
const (
	childDone   = 0
	childFailed = 1
	childBroken = 2
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		// A generated test main may put flags of its own before the
		// arguments.
		var args []string
		for _, arg := range os.Args[1:] {
			if !strings.HasPrefix(arg, "-test.") {
				args = append(args, arg)
			}
		}
		os.Exit(child(mode, args))
	}
	os.Exit(m.Run())
}

func child(mode string, args []string) int {
	var err error
	switch {
	case mode == "dial" && len(args) == 1:
		var conn net.Conn
		if conn, err = net.DialTimeout("tcp", args[0], attemptDeadline); err == nil {
			conn.Close()
		}
	case mode == "listen" && len(args) == 1:
		var l net.Listener
		if l, err = net.Listen("tcp", args[0]); err == nil {
			l.Close()
		}
	case mode == "lookup" && len(args) == 1:
		ctx, cancel := context.WithTimeout(context.Background(), attemptDeadline)
		defer cancel()
		_, err = net.DefaultResolver.LookupHost(ctx, args[0])
	case mode == "request" && len(args) == 3:
		return request(args[0], args[1], args[2])
	default:
		fmt.Printf("unknown attempt %q %q\n", mode, args)
		return childBroken
	}
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		return childFailed
	}
	fmt.Println("done")
	return childDone
}

// request sends one request the way a tool does that honours the proxy
// variables, and prints the status and the body of the answer.
func request(method, url, body string) int {
	var content io.Reader
	if body != "" {
		content = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, content)
	if err != nil {
		fmt.Printf("bad request: %v\n", err)
		return childBroken
	}
	client := &http.Client{
		Timeout: attemptDeadline,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// The local server stands in for the host, under a
			// certificate of its own.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Do(req)
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

// names resolves the hosts of the suite and no other, off the network.
type names map[string]string

func (n names) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addr, ok := n[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []netip.Addr{netip.MustParseAddr(addr)}, nil
}

// world is a proxy in front of two local servers, and the profile of a
// command that may reach the proxy.
type world struct {
	proxy   string
	profile confine.Profile
	self    string

	mu       sync.Mutex
	refusals []egress.Refusal
	served   []string
}

func (w *world) refused() []egress.Refusal {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]egress.Refusal{}, w.refusals...)
}

func (w *world) requests() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string{}, w.served...)
}

func newWorld(t *testing.T) *world {
	t.Helper()
	requireBackend(t)
	w := &world{}

	handler := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.served = append(w.served, r.Method+" "+r.Host+r.URL.Path)
		w.mu.Unlock()
		io.WriteString(rw, downloaded)
	})
	plain := httptest.NewServer(handler)
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(handler)
	t.Cleanup(secure.Close)
	behind := map[string]string{
		allowedAddr + ":80":  plain.Listener.Addr().String(),
		allowedAddr + ":443": secure.Listener.Addr().String(),
	}

	p, err := egress.New(egress.Config{
		Allow: []string{allowedHost, privateHost, privateAddr},
		OnRefuse: func(r egress.Refusal) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.refusals = append(w.refusals, r)
		},
		Resolver: names{allowedHost: allowedAddr, privateHost: privateAddr},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			local, ok := behind[address]
			if !ok {
				return nil, fmt.Errorf("the test has no server for %s", address)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, local)
		},
	})
	if err != nil {
		t.Fatalf("egress.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if w.proxy, err = p.Start(ctx); err != nil {
		t.Fatalf("start the proxy: %v", err)
	}
	t.Cleanup(func() { p.Close() })

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}
	if w.self, err = filepath.EvalSymlinks(self); err != nil {
		t.Fatalf("resolve %s: %v", self, err)
	}
	w.profile = confine.Profile{
		ReadOnly: []string{w.self},
		Network:  confine.Proxy(w.proxy),
	}
	return w
}

type outcome struct {
	status int
	output string
}

// attempt runs the test binary as a confined command that makes one attempt.
// The attempt must have been made: a command that did not get that far fails
// the test.
func (w *world) attempt(t *testing.T, profile confine.Profile, mode string, args ...string) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	profile.Env.Set = map[string]string{childEnv: mode}
	cmd, err := confine.Command(ctx, profile, w.self, args...)
	if err != nil {
		t.Fatalf("Command %s %q: %v", mode, args, err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return outcome{status: childDone, output: out.String()}
	case errors.As(err, &exit) && exit.ExitCode() == childFailed:
		return outcome{status: childFailed, output: out.String()}
	}
	t.Fatalf("%s %q did not make its attempt: %v: %q", mode, args, err, out.String())
	return outcome{}
}

func (w *world) get(t *testing.T, url string) outcome {
	t.Helper()
	return w.attempt(t, w.profile, "request", http.MethodGet, url, "")
}

// wantRefusals checks what reached OnRefuse and that the servers behind the
// proxy saw nothing of it.
func (w *world) wantRefusals(t *testing.T, want ...egress.Refusal) {
	t.Helper()
	if got := w.refused(); len(got)+len(want) > 0 && !reflect.DeepEqual(got, want) {
		t.Errorf("OnRefuse received %+v, want %+v", got, want)
	}
	if got := w.requests(); len(got) != 0 {
		t.Errorf("the servers behind the proxy received %q, want nothing", got)
	}
}

// wantForbidden checks the answer of the proxy to a refused request.
func wantForbidden(t *testing.T, r outcome, reason egress.Reason) {
	t.Helper()
	want := fmt.Sprintf("status %d: egress: %s\n", http.StatusForbidden, reason)
	if r.status != childFailed || r.output != want {
		t.Errorf("got status %d and %q, want the answer %q", r.status, r.output, want)
	}
}

func TestProxyIsReachable(t *testing.T) {
	w := newWorld(t)
	if r := w.attempt(t, w.profile, "dial", w.proxy); r.status != childDone {
		t.Errorf("connect to the proxy at %s: got %q, want it to succeed", w.proxy, r.output)
	}
}

func TestDownloadFromAnAllowedHost(t *testing.T) {
	w := newWorld(t)
	want := fmt.Sprintf("status %d: %s\n", http.StatusOK, downloaded)
	for _, scheme := range []string{"http", "https"} {
		url := scheme + "://" + allowedHost + "/file"
		if r := w.get(t, url); r.status != childDone || r.output != want {
			t.Errorf("download of %s: got status %d and %q, want %q", url, r.status, r.output, want)
		}
	}
	served := []string{"GET " + allowedHost + "/file", "GET " + allowedHost + "/file"}
	if got := w.requests(); !reflect.DeepEqual(got, served) {
		t.Errorf("the servers behind the proxy received %q, want %q", got, served)
	}
	if got := w.refused(); len(got) != 0 {
		t.Errorf("OnRefuse received %+v, want nothing", got)
	}
}

func TestEscapeConnectToAPublicAddress(t *testing.T) {
	w := newWorld(t)
	r := w.attempt(t, w.profile, "dial", publicAddr)
	if r.status != childFailed {
		t.Errorf("connect to %s: want it to fail, got %q", publicAddr, r.output)
	}
	t.Logf("connect to %s: %s", publicAddr, r.output)
}

func TestEscapeConnectToAnotherLoopbackPort(t *testing.T) {
	w := newWorld(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	other := l.Addr().String()
	r := w.attempt(t, w.profile, "dial", other)
	if r.status != childFailed {
		t.Errorf("connect to %s: want it to fail, got %q", other, r.output)
	}
	// The control: the listener is there, and only the profile is in the
	// way. The first connection it accepts is this one.
	conn, err := net.DialTimeout("tcp", other, attemptDeadline)
	if err != nil {
		t.Fatalf("connect to %s from outside the sandbox: %v", other, err)
	}
	defer conn.Close()
	first, err := l.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer first.Close()
	if got, want := first.RemoteAddr().String(), conn.LocalAddr().String(); got != want {
		t.Errorf("the listener at %s accepted a connection from %s before the one from %s", other, got, want)
	}
}

func TestEscapeListen(t *testing.T) {
	w := newWorld(t)
	for _, addr := range []string{"127.0.0.1:0", "0.0.0.0:0", "[::1]:0"} {
		if r := w.attempt(t, w.profile, "listen", addr); r.status != childFailed {
			t.Errorf("listen on %s: want it to fail, got %q", addr, r.output)
		}
	}
}

func TestEscapeResolveAName(t *testing.T) {
	w := newWorld(t)
	r := w.attempt(t, w.profile, "lookup", publicName)
	if r.status != childFailed {
		t.Errorf("lookup of %s: want it to fail, got %q", publicName, r.output)
	}
	t.Logf("lookup of %s: %s", publicName, r.output)
}

func TestEscapePrivateAddressThroughTheProxy(t *testing.T) {
	w := newWorld(t)
	wantForbidden(t, w.get(t, "http://"+privateAddr+"/"), egress.AddressNotAllowed)
	wantForbidden(t, w.get(t, "http://"+privateHost+"/"), egress.AddressNotAllowed)
	if r := w.get(t, "https://"+privateAddr+"/"); r.status != childFailed {
		t.Errorf("tunnel to %s: want it to fail, got %q", privateAddr, r.output)
	}
	w.wantRefusals(t,
		egress.Refusal{Host: privateAddr, Method: http.MethodGet, Reason: egress.AddressNotAllowed},
		egress.Refusal{Host: privateHost, Method: http.MethodGet, Reason: egress.AddressNotAllowed},
		egress.Refusal{Host: privateAddr, Method: http.MethodConnect, Reason: egress.AddressNotAllowed},
	)
}

func TestEscapeBodyThroughTheProxy(t *testing.T) {
	w := newWorld(t)
	url := "http://" + allowedHost + "/upload"
	wantForbidden(t, w.attempt(t, w.profile, "request", http.MethodGet, url, payload), egress.BodyNotAllowed)
	wantForbidden(t, w.attempt(t, w.profile, "request", http.MethodPost, url, payload), egress.MethodNotAllowed)
	w.wantRefusals(t,
		egress.Refusal{Host: allowedHost, Method: http.MethodGet, Reason: egress.BodyNotAllowed},
		egress.Refusal{Host: allowedHost, Method: http.MethodPost, Reason: egress.MethodNotAllowed},
	)
}

func TestEscapeUnlistedHostThroughTheProxy(t *testing.T) {
	w := newWorld(t)
	wantForbidden(t, w.get(t, "http://"+unlistedHost+"/"), egress.HostNotAllowed)
	if r := w.get(t, "https://"+unlistedHost+"/"); r.status != childFailed {
		t.Errorf("tunnel to %s: want it to fail, got %q", unlistedHost, r.output)
	}
	w.wantRefusals(t,
		egress.Refusal{Host: unlistedHost, Method: http.MethodGet, Reason: egress.HostNotAllowed},
		egress.Refusal{Host: unlistedHost, Method: http.MethodConnect, Reason: egress.HostNotAllowed},
	)
}

// With the network closed the proxy is one more destination out of reach.
func TestClosedNetworkCannotReachTheProxy(t *testing.T) {
	w := newWorld(t)
	closed := w.profile
	closed.Network = confine.None
	if r := w.attempt(t, closed, "dial", w.proxy); r.status != childFailed {
		t.Errorf("connect to the proxy at %s: want it to fail, got %q", w.proxy, r.output)
	}
	// A tool that was told of the proxy some other way gets no further.
	url := "http://" + w.proxy + "/"
	if r := w.attempt(t, closed, "request", http.MethodGet, url, ""); r.status != childFailed {
		t.Errorf("request to the proxy at %s: want it to fail, got %q", url, r.output)
	}
	w.wantRefusals(t)
}

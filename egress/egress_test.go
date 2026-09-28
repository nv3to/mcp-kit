package egress_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nv3to/mcp-kit/egress"
)

// public is the address every allowed name resolves to unless a test says
// otherwise. It is never connected to: the dialer goes to a local server.
const public = "93.184.216.34"

// world is the network as the proxy sees it: a resolver with fixed answers
// and a dialer that records the address it was given and connects to a local
// server instead.
type world struct {
	mu       sync.Mutex
	answers  map[string][]string
	lookups  []string
	dialled  []string
	refusals []egress.Refusal
	upstream string
}

func (w *world) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lookups = append(w.lookups, host)
	texts, ok := w.answers[host]
	if !ok {
		texts = []string{public}
	}
	// Every lookup after the first of a name gets the last answer alone, so
	// a test can make a name change between two resolutions.
	if n := len(texts); n > 1 && strings.HasPrefix(texts[0], "then:") {
		w.answers[host] = texts[n-1:]
		texts = []string{strings.TrimPrefix(texts[0], "then:")}
	}
	var addrs []netip.Addr
	for _, text := range texts {
		addrs = append(addrs, netip.MustParseAddr(text))
	}
	return addrs, nil
}

func (w *world) dial(ctx context.Context, network, address string) (net.Conn, error) {
	w.mu.Lock()
	w.dialled = append(w.dialled, address)
	w.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, network, w.upstream)
}

func (w *world) record(r egress.Refusal) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refusals = append(w.refusals, r)
}

func (w *world) refused() []egress.Refusal {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.refusals)
}

func (w *world) dials() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.dialled)
}

// start runs a proxy that allows pkg.example, every subdomain of
// mirror.example and the literal 93.184.216.35, in front of upstream.
func start(t *testing.T, upstream string, answers map[string][]string) (*world, string) {
	t.Helper()
	w := &world{answers: answers, upstream: upstream}
	p, err := egress.New(egress.Config{
		Allow:    []string{"pkg.example", "*.mirror.example", "93.184.216.35"},
		Resolver: w,
		Dial:     w.dial,
		OnRefuse: w.record,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return w, addr
}

// origin is a local server standing in for the allowed public hosts.
func origin(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s %s from %s", r.Method, r.URL.Path, r.Host)
		}
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// client sends its requests through the proxy and follows no redirect, so a
// test sees what the proxy itself answered.
func client(t *testing.T, proxy string) *http.Client {
	t.Helper()
	tr := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: proxy})}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func do(t *testing.T, c *http.Client, method, target string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(text)
}

// wantRefusal checks the answer the client got and the one refusal reported.
func wantRefusal(t *testing.T, w *world, status int, body string, want egress.Refusal) {
	t.Helper()
	if status != http.StatusForbidden || !strings.Contains(body, string(want.Reason)) {
		t.Errorf("got %d %q, want 403 naming %q", status, body, want.Reason)
	}
	if got := w.refused(); len(got) != 1 || got[0] != want {
		t.Errorf("OnRefuse got %+v, want one %+v", got, want)
	}
	if got := w.dials(); len(got) != 0 {
		t.Errorf("a refused request dialled %v", got)
	}
}

func TestGetToAllowedHost(t *testing.T) {
	for _, host := range []string{"pkg.example", "PKG.example", "eu.mirror.example", "a.b.mirror.example", "93.184.216.35"} {
		t.Run(host, func(t *testing.T) {
			w, proxy := start(t, origin(t, nil), nil)
			status, body := do(t, client(t, proxy), "GET", "http://"+host+"/mod/v1.zip", nil)
			if status != 200 || !strings.EqualFold(body, "GET /mod/v1.zip from "+host) {
				t.Errorf("got %d %q", status, body)
			}
			if got := w.refused(); len(got) != 0 {
				t.Errorf("OnRefuse got %+v", got)
			}
		})
	}
}

func TestHeadToAllowedHost(t *testing.T) {
	_, proxy := start(t, origin(t, nil), nil)
	if status, _ := do(t, client(t, proxy), "HEAD", "http://pkg.example/", nil); status != 200 {
		t.Errorf("got %d", status)
	}
}

func TestHostNotListedIsRefused(t *testing.T) {
	for _, host := range []string{"evil.example", "mirror.example", "pkg.example.evil.example", "93.184.216.34", "[2606:2800:220:1::1]"} {
		t.Run(host, func(t *testing.T) {
			w, proxy := start(t, origin(t, nil), nil)
			status, body := do(t, client(t, proxy), "GET", "http://"+host+"/", nil)
			wantRefusal(t, w, status, body, egress.Refusal{
				Host:   strings.Trim(host, "[]"),
				Method: "GET",
				Reason: egress.HostNotAllowed,
			})
		})
	}
}

func TestWritingMethodsAreRefused(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			w, proxy := start(t, origin(t, nil), nil)
			status, body := do(t, client(t, proxy), method, "http://pkg.example/", strings.NewReader("secret"))
			wantRefusal(t, w, status, body, egress.Refusal{Host: "pkg.example", Method: method, Reason: egress.MethodNotAllowed})
		})
	}
}

func TestGetWithBodyIsRefused(t *testing.T) {
	bodies := map[string]io.Reader{
		"content length": strings.NewReader("secret"),
		"chunked":        io.MultiReader(strings.NewReader("secret")),
	}
	for name, payload := range bodies {
		t.Run(name, func(t *testing.T) {
			w, proxy := start(t, origin(t, nil), nil)
			status, body := do(t, client(t, proxy), "GET", "http://pkg.example/", payload)
			wantRefusal(t, w, status, body, egress.Refusal{Host: "pkg.example", Method: "GET", Reason: egress.BodyNotAllowed})
		})
	}
}

func TestPlainRequestToAnotherPortIsRefused(t *testing.T) {
	w, proxy := start(t, origin(t, nil), nil)
	status, body := do(t, client(t, proxy), "GET", "http://pkg.example:8080/", nil)
	wantRefusal(t, w, status, body, egress.Refusal{Host: "pkg.example", Method: "GET", Reason: egress.PortNotAllowed})
}

func TestForbiddenAddressIsRefused(t *testing.T) {
	addrs := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.1.1",
		"169.254.169.254", "100.100.100.200", "0.0.0.0", "224.0.0.1",
		"::1", "fe80::1", "fd00::1", "fd00:ec2::254", "::", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::7f00:1",
	}
	for _, addr := range addrs {
		t.Run(addr, func(t *testing.T) {
			w, proxy := start(t, origin(t, nil), map[string][]string{"pkg.example": {addr}})
			status, body := do(t, client(t, proxy), "GET", "http://pkg.example/", nil)
			wantRefusal(t, w, status, body, egress.Refusal{Host: "pkg.example", Method: "GET", Reason: egress.AddressNotAllowed})
		})
	}
}

func TestOneForbiddenAddressAmongManyIsRefused(t *testing.T) {
	w, proxy := start(t, origin(t, nil), map[string][]string{"pkg.example": {public, "127.0.0.1"}})
	status, body := do(t, client(t, proxy), "GET", "http://pkg.example/", nil)
	wantRefusal(t, w, status, body, egress.Refusal{Host: "pkg.example", Method: "GET", Reason: egress.AddressNotAllowed})
}

// A listed literal address is still subject to the address check.
func TestListedForbiddenAddressIsRefused(t *testing.T) {
	w := &world{upstream: origin(t, nil)}
	p, err := egress.New(egress.Config{
		Allow:    []string{"127.0.0.1"},
		Resolver: w,
		Dial:     w.dial,
		OnRefuse: w.record,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	status, body := do(t, client(t, proxy), "GET", "http://127.0.0.1/", nil)
	wantRefusal(t, w, status, body, egress.Refusal{Host: "127.0.0.1", Method: "GET", Reason: egress.AddressNotAllowed})
}

// The name resolves to a public address when checked and to loopback ever
// after. The proxy dials the address it checked, so the change finds nothing
// to act on.
func TestResolutionChangingAfterTheCheck(t *testing.T) {
	w, proxy := start(t, origin(t, nil), map[string][]string{"pkg.example": {"then:" + public, "127.0.0.1"}})
	c := client(t, proxy)
	if status, body := do(t, c, "GET", "http://pkg.example/", nil); status != 200 {
		t.Fatalf("got %d %q", status, body)
	}
	if got := w.dials(); !slices.Equal(got, []string{public + ":80"}) {
		t.Errorf("dialled %v, want the checked address alone", got)
	}
	w.mu.Lock()
	lookups := slices.Clone(w.lookups)
	w.mu.Unlock()
	if !slices.Equal(lookups, []string{"pkg.example"}) {
		t.Errorf("resolved %v, want one lookup: the dial must not resolve again", lookups)
	}

	// A tunnel is a new connection, resolved anew, and now refused.
	status, body := connect(t, proxy, "pkg.example:443").status()
	if status != http.StatusForbidden || !strings.Contains(body, string(egress.AddressNotAllowed)) {
		t.Errorf("got %d %q", status, body)
	}
	if got := w.dials(); len(got) != 1 {
		t.Errorf("dialled %v: the forbidden address was reached", got)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	upstream := origin(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example/next", http.StatusFound)
	})
	w, proxy := start(t, upstream, nil)
	req, err := http.NewRequest("GET", "http://pkg.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client(t, proxy).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "http://evil.example/next" {
		t.Errorf("got %d to %q, want the redirect itself", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got := w.dials(); len(got) != 1 {
		t.Errorf("dialled %v, want one connection", got)
	}
	if got := w.refused(); len(got) != 0 {
		t.Errorf("OnRefuse got %+v", got)
	}
}

func TestHopByHopHeadersAreDropped(t *testing.T) {
	seen := make(chan http.Header, 1)
	upstream := origin(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Kept", "yes")
	})
	_, proxy := start(t, upstream, nil)
	req, err := http.NewRequest("GET", "http://pkg.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "X-Private")
	req.Header.Set("X-Private", "1")
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Accept", "text/plain")
	resp, err := client(t, proxy).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-seen
	for _, name := range []string{"Connection", "X-Private", "Proxy-Authorization", "Upgrade"} {
		if _, ok := got[name]; ok {
			t.Errorf("the origin received %s", name)
		}
	}
	if got.Get("Accept") != "text/plain" {
		t.Errorf("the origin lost Accept: %v", got)
	}
	if resp.Header.Get("Keep-Alive") != "" || resp.Header.Get("X-Kept") != "yes" {
		t.Errorf("the client received %v", resp.Header)
	}
}

// tunnel is a raw connection to the proxy that has sent a CONNECT.
type tunnel struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func connect(t *testing.T, proxy, target string) *tunnel {
	t.Helper()
	conn, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	return &tunnel{t: t, conn: conn, r: bufio.NewReader(conn)}
}

// status reads the proxy's answer to the CONNECT.
func (tn *tunnel) status() (int, string) {
	tn.t.Helper()
	resp, err := http.ReadResponse(tn.r, &http.Request{Method: "CONNECT"})
	if err != nil {
		tn.t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// echo is a server that is not HTTP: it sends back what it receives, in
// upper case, which shows the tunnel carries bytes both ways untouched.
func echo(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				lines := bufio.NewScanner(conn)
				for lines.Scan() {
					fmt.Fprintln(conn, strings.ToUpper(lines.Text()))
				}
			}()
		}
	}()
	return l.Addr().String()
}

func TestConnectTunnelsToPort443(t *testing.T) {
	w, proxy := start(t, echo(t), nil)
	tn := connect(t, proxy, "pkg.example:443")
	if status, body := tn.status(); status != 200 {
		t.Fatalf("got %d %q", status, body)
	}
	for _, line := range []string{"hello", "again"} {
		if _, err := fmt.Fprintln(tn.conn, line); err != nil {
			t.Fatal(err)
		}
		got, err := tn.r.ReadString('\n')
		if err != nil || got != strings.ToUpper(line)+"\n" {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	if got := w.dials(); !slices.Equal(got, []string{public + ":443"}) {
		t.Errorf("dialled %v", got)
	}
}

func TestConnectIsRefused(t *testing.T) {
	cases := []struct {
		target string
		want   egress.Refusal
	}{
		{"pkg.example:80", egress.Refusal{Host: "pkg.example", Method: "CONNECT", Reason: egress.PortNotAllowed}},
		{"pkg.example:22", egress.Refusal{Host: "pkg.example", Method: "CONNECT", Reason: egress.PortNotAllowed}},
		{"evil.example:443", egress.Refusal{Host: "evil.example", Method: "CONNECT", Reason: egress.HostNotAllowed}},
		{"127.0.0.1:443", egress.Refusal{Host: "127.0.0.1", Method: "CONNECT", Reason: egress.HostNotAllowed}},
		{"internal.mirror.example:443", egress.Refusal{Host: "internal.mirror.example", Method: "CONNECT", Reason: egress.AddressNotAllowed}},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			w, proxy := start(t, echo(t), map[string][]string{"internal.mirror.example": {"10.1.2.3"}})
			status, body := connect(t, proxy, tc.target).status()
			wantRefusal(t, w, status, body, tc.want)
		})
	}
}

func TestCloseEndsATunnel(t *testing.T) {
	w := &world{upstream: echo(t)}
	p, err := egress.New(egress.Config{Allow: []string{"pkg.example"}, Resolver: w, Dial: w.dial})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tn := connect(t, proxy, "pkg.example:443")
	if status, body := tn.status(); status != 200 {
		t.Fatalf("got %d %q", status, body)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tn.r.ReadByte(); err == nil {
		t.Error("the tunnel outlived the proxy")
	}
}

func TestIdleTunnelIsClosed(t *testing.T) {
	w := &world{upstream: echo(t)}
	p, err := egress.New(egress.Config{
		Allow:       []string{"pkg.example"},
		Resolver:    w,
		Dial:        w.dial,
		IdleTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	tn := connect(t, proxy, "pkg.example:443")
	if status, body := tn.status(); status != 200 {
		t.Fatalf("got %d %q", status, body)
	}
	if _, err := tn.r.ReadByte(); err != io.EOF {
		t.Errorf("got %v, want the proxy to close the idle tunnel", err)
	}
}

func TestServeOnASuppliedListener(t *testing.T) {
	w := &world{upstream: origin(t, nil)}
	p, err := egress.New(egress.Config{Allow: []string{"pkg.example"}, Resolver: w, Dial: w.dial})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- p.Serve(l) }()
	if status, body := do(t, client(t, l.Addr().String()), "GET", "http://pkg.example/", nil); status != 200 {
		t.Errorf("got %d %q", status, body)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve returned %v after Close", err)
	}
}

func TestStartEndsWithItsContext(t *testing.T) {
	w := &world{upstream: origin(t, nil)}
	p, err := egress.New(egress.Config{Allow: []string{"pkg.example"}, Resolver: w, Dial: w.dial})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr, err := p.Start(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if host, _, _ := net.SplitHostPort(addr); host != "127.0.0.1" {
		t.Errorf("listening on %s, want loopback", addr)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the proxy still listens after its context ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOversizedHeaderIsRejected(t *testing.T) {
	w := &world{upstream: origin(t, nil)}
	p, err := egress.New(egress.Config{Allow: []string{"pkg.example"}, Resolver: w, Dial: w.dial, MaxHeaderBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	req, err := http.NewRequest("GET", "http://pkg.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Padding", strings.Repeat("a", 1<<20))
	resp, err := client(t, proxy).Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Errorf("got %d", resp.StatusCode)
		}
	}
	if got := w.dials(); len(got) != 0 {
		t.Errorf("dialled %v", got)
	}
}

func TestConnectionsAreBounded(t *testing.T) {
	w := &world{upstream: echo(t)}
	p, err := egress.New(egress.Config{Allow: []string{"pkg.example"}, Resolver: w, Dial: w.dial, MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	first := connect(t, proxy, "pkg.example:443")
	if status, body := first.status(); status != 200 {
		t.Fatalf("got %d %q", status, body)
	}

	// The second client connects, but is not served while the first holds
	// the only slot.
	second := connect(t, proxy, "pkg.example:443")
	_ = second.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := second.r.ReadByte(); err == nil {
		t.Fatal("a second connection was served beyond the bound")
	}
	_ = first.conn.Close()
	_ = second.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if status, body := second.status(); status != 200 {
		t.Errorf("got %d %q once the slot was free", status, body)
	}
}

func TestNewRefusesABadAllowlist(t *testing.T) {
	for _, entry := range []string{"", "*", "*.", "pkg.*.example", "http://pkg.example", "pkg.example:443", "pkg.example/path", "*.10.0.0.1"} {
		if _, err := egress.New(egress.Config{Allow: []string{entry}}); err == nil {
			t.Errorf("New accepted the allowed host %q", entry)
		}
	}
	if _, err := egress.New(egress.Config{MaxConns: -1}); err == nil {
		t.Error("New accepted a negative limit")
	}
}

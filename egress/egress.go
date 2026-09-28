// Package egress is a forward proxy that lets a confined command read from
// named hosts and nothing else. An open network turns the host into a pivot
// and a closed one makes every download a human's job; the proxy is the one
// destination in between. It reports each refusal to its owner, so a server
// can tell its caller what was refused and why.
//
// The proxy knows nothing of confinement: pointing the command at it, and
// closing every other route, is the caller's business.
package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Reason says why the proxy refused a request. The set is closed, so a caller
// can branch on it.
type Reason string

// The reasons for a refusal.
const (
	// HostNotAllowed: the host matches no entry of Config.Allow.
	HostNotAllowed Reason = "host not allowed"

	// MethodNotAllowed: the method is not GET, HEAD or CONNECT.
	MethodNotAllowed Reason = "method not allowed"

	// BodyNotAllowed: the request carries a body or announces one.
	BodyNotAllowed Reason = "body not allowed"

	// AddressNotAllowed: the host is, or resolves to, a forbidden address.
	AddressNotAllowed Reason = "address not allowed"

	// PortNotAllowed: the port is not 80 for plain HTTP or 443 for CONNECT.
	PortNotAllowed Reason = "port not allowed"
)

// Refusal describes one refused request, as handed to Config.OnRefuse.
type Refusal struct {
	// Host is the host the request named, without its port.
	Host string

	// Method is the request's method.
	Method string

	// Reason is why the proxy refused.
	Reason Reason
}

// Resolver looks up the addresses of a host name. *net.Resolver implements
// it; tests replace it to stay off the network.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Defaults for the limits a Config leaves at zero.
const (
	DefaultMaxHeaderBytes = 64 << 10
	DefaultIdleTimeout    = 2 * time.Minute
	DefaultMaxConns       = 64
)

// Config configures a Proxy.
type Config struct {
	// Allow lists the hosts the proxy may reach: exact names, "*.suffix"
	// patterns that match every subdomain of suffix but not suffix itself,
	// and literal addresses. Names are compared without regard to case. A
	// literal address is refused unless it is listed, and listing a forbidden
	// address does not make it reachable.
	Allow []string

	// OnRefuse, if set, is called once for every refused request, before the
	// client gets its answer. It is called from the goroutine that serves the
	// request, so it must be safe for concurrent use.
	OnRefuse func(Refusal)

	// Resolver resolves host names; nil means net.DefaultResolver.
	Resolver Resolver

	// Dial opens the upstream connection. It is always given a literal
	// address the proxy has checked, never a name, so a name cannot change
	// between the check and the dial. Nil means a net.Dialer that checks the
	// address once more as the socket connects. A replacement is trusted with
	// where it really connects, which lets a test stand a local server in for
	// a public host.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	// MaxHeaderBytes bounds a request's header; zero means
	// DefaultMaxHeaderBytes.
	MaxHeaderBytes int

	// IdleTimeout closes a tunnel or a client connection that has carried
	// nothing for this long, and bounds the time a client may take to send
	// its header; zero means DefaultIdleTimeout.
	IdleTimeout time.Duration

	// MaxConns bounds the client connections served at once; further clients
	// wait. Zero means DefaultMaxConns.
	MaxConns int
}

// Proxy is a forward proxy that serves GET and HEAD over plain HTTP on port
// 80, and opaque CONNECT tunnels to port 443, for the hosts of its Config. It
// never follows a redirect: the client does, and each hop is checked as a
// request of its own.
type Proxy struct {
	cfg       Config
	exact     map[string]bool
	suffixes  []string
	transport *http.Transport
	server    *http.Server
	slots     chan struct{}

	mu      sync.Mutex
	tunnels map[net.Conn]struct{}
	closed  bool
}

// New returns a proxy for cfg. It fails on an entry of cfg.Allow that is
// neither a host name, a "*.suffix" pattern nor a literal address, so a
// mistyped allowlist is found at start-up and not as a refusal later.
func New(cfg Config) (*Proxy, error) {
	if cfg.MaxHeaderBytes < 0 || cfg.IdleTimeout < 0 || cfg.MaxConns < 0 {
		return nil, errors.New("egress: a limit must not be negative")
	}
	if cfg.MaxHeaderBytes == 0 {
		cfg.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.MaxConns == 0 {
		cfg.MaxConns = DefaultMaxConns
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.Dial == nil {
		cfg.Dial = (&net.Dialer{Timeout: 30 * time.Second, Control: control}).DialContext
	}
	p := &Proxy{
		cfg:     cfg,
		exact:   map[string]bool{},
		slots:   make(chan struct{}, cfg.MaxConns),
		tunnels: map[net.Conn]struct{}{},
	}
	for _, entry := range cfg.Allow {
		pattern, wildcard := strings.CutPrefix(entry, "*.")
		host := normalise(pattern)
		if host == "" || strings.ContainsAny(host, "*/:@ ") && !isAddr(host) {
			return nil, fmt.Errorf("egress: allowed host %q is not a name, a *.suffix pattern or an address", entry)
		}
		if !wildcard {
			p.exact[host] = true
			continue
		}
		if isAddr(host) {
			return nil, fmt.Errorf("egress: allowed host %q puts a wildcard on an address", entry)
		}
		p.suffixes = append(p.suffixes, "."+host)
	}
	// A transport of its own: the default one takes a proxy from the
	// environment, and compression would rewrite what the client receives.
	p.transport = &http.Transport{
		DialContext:            p.dial,
		DisableCompression:     true,
		MaxIdleConns:           cfg.MaxConns,
		IdleConnTimeout:        cfg.IdleTimeout,
		ResponseHeaderTimeout:  cfg.IdleTimeout,
		MaxResponseHeaderBytes: int64(cfg.MaxHeaderBytes),
	}
	p.server = &http.Server{
		Handler:           http.HandlerFunc(p.handle),
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ReadHeaderTimeout: cfg.IdleTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
	return p, nil
}

// Start serves on a loopback port the system chooses and returns its address
// as host:port. The proxy closes when ctx ends or Close is called.
func (p *Proxy) Start(ctx context.Context) (string, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("egress: listen: %w", err)
	}
	go func() { _ = p.Serve(l) }()
	context.AfterFunc(ctx, func() { _ = p.Close() })
	return l.Addr().String(), nil
}

// Serve serves on a listener the caller supplies, such as a unix socket, and
// blocks until the proxy is closed or the listener fails. It closes l before
// it returns, and returns nil when the cause was Close.
func (p *Proxy) Serve(l net.Listener) error {
	err := p.server.Serve(&limitListener{Listener: l, slots: p.slots, done: make(chan struct{})})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops every listener and closes every connection, tunnels included.
// A closed proxy cannot be started again.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	for c := range p.tunnels {
		_ = c.Close()
	}
	p.mu.Unlock()
	err := p.server.Close()
	p.transport.CloseIdleConnections()
	return err
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.Host == "" || r.URL.Scheme != "http" {
		http.Error(w, "egress: not a plain HTTP proxy request", http.StatusBadRequest)
		return
	}
	host := normalise(r.URL.Hostname())
	switch {
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		p.refuse(w, r, host, MethodNotAllowed)
		return
	case r.ContentLength != 0 || len(r.TransferEncoding) > 0:
		p.refuse(w, r, host, BodyNotAllowed)
		return
	case !p.allowed(host):
		p.refuse(w, r, host, HostNotAllowed)
		return
	case r.URL.Port() != "" && r.URL.Port() != "80":
		p.refuse(w, r, host, PortNotAllowed)
		return
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), nil)
	if err != nil {
		http.Error(w, "egress: bad request", http.StatusBadRequest)
		return
	}
	copyHeader(out.Header, r.Header)
	if _, ok := out.Header["User-Agent"]; !ok {
		// Keep the transport from adding its own.
		out.Header.Set("User-Agent", "")
	}
	// RoundTrip, not a client: a redirect goes back to the caller as it is.
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		p.fail(w, r, host, err)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	name, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "egress: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	host := normalise(name)
	switch {
	case r.ContentLength != 0 || len(r.TransferEncoding) > 0:
		p.refuse(w, r, host, BodyNotAllowed)
		return
	case !p.allowed(host):
		p.refuse(w, r, host, HostNotAllowed)
		return
	case port != "443":
		p.refuse(w, r, host, PortNotAllowed)
		return
	}
	upstream, err := p.dial(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		p.fail(w, r, host, err)
		return
	}
	defer upstream.Close()

	client, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "egress: cannot tunnel on this connection", http.StatusInternalServerError)
		return
	}
	defer client.Close()
	if !p.track(client, upstream) {
		return
	}
	defer p.untrack(client, upstream)

	// The server's deadlines do not apply to a tunnel; pipe sets its own.
	_ = client.SetDeadline(time.Now().Add(p.cfg.IdleTimeout))
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// What the client sent behind its header is already in the buffer.
	if n := buf.Reader.Buffered(); n > 0 {
		early, _ := buf.Reader.Peek(n)
		if _, err := upstream.Write(early); err != nil {
			return
		}
	}

	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.pipe(upstream, client, &last)
	}()
	go func() {
		defer wg.Done()
		p.pipe(client, upstream, &last)
	}()
	wg.Wait()
}

// pipe copies src to dst until src ends, a write fails or the tunnel has
// been idle in both directions for the idle timeout. last is the time of the
// tunnel's latest traffic, shared by both directions, so a long download is
// not cut off because the client has nothing to say.
func (p *Proxy) pipe(dst, src net.Conn, last *atomic.Int64) {
	idle := p.cfg.IdleTimeout
	buf := make([]byte, 32<<10)
	for {
		_ = src.SetReadDeadline(time.Now().Add(idle))
		n, err := src.Read(buf)
		if n > 0 {
			last.Store(time.Now().UnixNano())
			_ = dst.SetWriteDeadline(time.Now().Add(idle))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				_ = src.Close()
				return
			}
		}
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, last.Load())) < idle {
			continue
		}
		if half, ok := dst.(interface{ CloseWrite() error }); ok && errors.Is(err, io.EOF) {
			_ = half.CloseWrite()
		} else {
			_ = dst.Close()
		}
		return
	}
}

func (p *Proxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	for _, c := range conns {
		p.tunnels[c] = struct{}{}
	}
	return true
}

func (p *Proxy) untrack(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		delete(p.tunnels, c)
	}
}

// refuse reports the refusal and tells the client the reason in a short
// plain response, so the tool behind the proxy sees why it failed.
func (p *Proxy) refuse(w http.ResponseWriter, r *http.Request, host string, reason Reason) {
	if p.cfg.OnRefuse != nil {
		p.cfg.OnRefuse(Refusal{Host: host, Method: r.Method, Reason: reason})
	}
	http.Error(w, "egress: "+string(reason), http.StatusForbidden)
}

// fail answers a request whose upstream connection could not be made: a
// refusal if the address check caused it, a gateway error otherwise.
func (p *Proxy) fail(w http.ResponseWriter, r *http.Request, host string, err error) {
	var refused *refusalError
	if errors.As(err, &refused) {
		p.refuse(w, r, host, refused.reason)
		return
	}
	http.Error(w, "egress: cannot reach "+host, http.StatusBadGateway)
}

func (p *Proxy) allowed(host string) bool {
	if p.exact[host] {
		return true
	}
	if isAddr(host) {
		return false
	}
	for _, suffix := range p.suffixes {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

// normalise gives a host the form names are compared in: lower case, no
// trailing dot, and an address in its canonical spelling.
func normalise(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().String()
	}
	return host
}

func isAddr(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// hopByHop are the headers that belong to one connection and are not
// forwarded, in either direction.
var hopByHop = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// copyHeader copies src to dst without the hop-by-hop headers and those the
// Connection header names.
func copyHeader(dst, src http.Header) {
	drop := map[string]bool{}
	for _, name := range hopByHop {
		drop[name] = true
	}
	for _, value := range src.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			drop[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range src {
		if drop[http.CanonicalHeaderKey(name)] {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}

// limitListener bounds the connections accepted and not yet closed. The
// slots are the proxy's, so the bound holds across all its listeners.
type limitListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: c, slots: l.slots}, nil
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	slots chan struct{}
	once  sync.Once
}

func (c *limitConn) Close() error {
	c.once.Do(func() { <-c.slots })
	return c.Conn.Close()
}

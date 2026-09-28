package confine

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// forwardArg is the first argument that makes Init run the forwarder
// instead of the server.
const forwardArg = "mcpkit-confine-forward"

// forwardFailed is the status of a forwarder that could not start the
// command.
const forwardFailed = 125

// relaySocket is the name of the unix socket in the directory of a relay.
const relaySocket = "proxy"

// socketMax is the longest path of a unix socket on the systems confine
// runs on: the address holds 104 bytes on macOS and 108 on Linux, the last a
// zero.
const socketMax = 103

// relayDial bounds the connection of the relay to the proxy.
const relayDial = 10 * time.Second

// initialized is set by Init.
var initialized atomic.Bool

// Init lets a confined command on Linux reach its proxy. A server that
// passes Proxy to Command on Linux calls it first in main, before anything
// else runs. There the command has a network namespace of its own, and a
// forwarder inside it carries its connections to the proxy: the forwarder is
// this program started again inside the sandbox, and Init is where it runs
// and ends. Without the call, Command refuses a Proxy on Linux. On macOS the
// sandbox reaches the proxy itself, and Init has nothing to do.
func Init() {
	if len(os.Args) > 1 && os.Args[1] == forwardArg {
		os.Exit(forward(os.Args[2:]))
	}
	initialized.Store(true)
}

// forwarder returns the program that runs the forwarder: this one, once
// Init was called.
func forwarder() (string, error) {
	if !initialized.Load() {
		return "", errors.New("a proxy on Linux needs confine.Init called first in main, where the forwarder runs that carries the connections of the command to the proxy")
	}
	if err := forwardable(); err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("the forwarder: %v", err)
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", fmt.Errorf("the forwarder: %v", err)
	}
	return self, nil
}

// relay carries every connection to its unix socket on to the proxy on the
// loopback of the host. It is the only way out of the network namespace of
// a command.
type relay struct {
	dir    string
	socket string
	proxy  string
	l      net.Listener
	wg     sync.WaitGroup

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// startRelay listens on a unix socket in a new directory of its own. The
// temporary directory of the system may lie too deep for the path of a unix
// socket, and then the directory is made in /tmp.
func startRelay(proxy netip.AddrPort) (*relay, error) {
	why := fmt.Errorf("no temporary directory holds a unix socket in at most %d bytes", socketMax)
	for _, parent := range []string{os.TempDir(), "/tmp"} {
		parent, err := filepath.EvalSymlinks(parent)
		if err != nil {
			why = err
			continue
		}
		dir, err := os.MkdirTemp(parent, "confine-proxy-")
		if err != nil {
			why = err
			continue
		}
		socket := filepath.Join(dir, relaySocket)
		if len(socket) > socketMax {
			os.Remove(dir)
			continue
		}
		l, err := net.Listen("unix", socket)
		if err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
		r := &relay{dir: dir, socket: socket, proxy: proxy.String(), l: l, conns: map[net.Conn]struct{}{}}
		r.wg.Add(1)
		go r.serve()
		return r, nil
	}
	return nil, why
}

func (r *relay) serve() {
	defer r.wg.Done()
	for {
		in, err := r.l.Accept()
		if err != nil {
			return
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.carry(in)
		}()
	}
}

// carry joins one connection to the socket to a connection of its own to
// the proxy. The proxy is the only address the relay dials.
func (r *relay) carry(in net.Conn) {
	out, err := net.DialTimeout("tcp", r.proxy, relayDial)
	if err != nil {
		in.Close()
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		in.Close()
		out.Close()
		return
	}
	r.conns[in], r.conns[out] = struct{}{}, struct{}{}
	r.mu.Unlock()
	join(in, out)
	r.mu.Lock()
	delete(r.conns, in)
	delete(r.conns, out)
	r.mu.Unlock()
}

// close stops listening, closes every connection and removes the directory
// of the socket.
func (r *relay) close() error {
	r.l.Close()
	r.mu.Lock()
	r.closed = true
	for c := range r.conns {
		c.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
	return os.RemoveAll(r.dir)
}

// join copies between a and b in both directions until both have ended. It
// passes an end of one direction on as a half close, which a tunnel through
// the proxy relies on.
func join(a, b net.Conn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(a, b)
		closeWrite(a)
	}()
	io.Copy(b, a)
	closeWrite(b)
	<-done
	a.Close()
	b.Close()
}

func closeWrite(c net.Conn) {
	if half, ok := c.(interface{ CloseWrite() error }); ok {
		half.CloseWrite()
		return
	}
	c.Close()
}

package mcpkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Serve runs the server on stdin and stdout and returns when stdin closes
// (nil), or when ctx is done (ctx's error).
//
// Stdout belongs to the protocol: for the duration of Serve, os.Stdout is
// pointed at stderr, so a stray fmt.Println in a handler cannot corrupt a
// frame, and only the transport holds the real stdout.
//
// A closed stdin does not cut a request short: the SDK ends the session as
// soon as it reads EOF, which would drop the response to a request that
// arrived just before it (`printf '<initialize>' | server`). Serve therefore
// holds the EOF back until every request it has read is answered.
func (s *Server) Serve(ctx context.Context) error {
	stdout := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = stdout }()

	calls := &pending{ids: map[string]bool{}}
	t := &mcp.IOTransport{
		Reader: io.NopCloser(&drainReader{ctx: ctx, r: os.Stdin, calls: calls}),
		Writer: nopWriteCloser{&responseWriter{w: stdout, calls: calls}},
	}
	err := s.srv.Run(ctx, t)
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, mcp.ErrConnectionClosed) {
		return nil
	}
	return err
}

// nopWriteCloser keeps the transport from closing the process's stdout.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// pending is the set of requests read from stdin and not yet answered on
// stdout, by JSON-RPC id.
type pending struct {
	mu     sync.Mutex
	ids    map[string]bool
	waiter chan struct{} // closed when ids becomes empty, if someone waits
}

func (p *pending) add(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids[id] = true
}

func (p *pending) done(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.ids, id)
	if len(p.ids) == 0 && p.waiter != nil {
		close(p.waiter)
		p.waiter = nil
	}
}

// drained blocks until no request is pending, or ctx is done.
func (p *pending) drained(ctx context.Context) {
	p.mu.Lock()
	if len(p.ids) == 0 {
		p.mu.Unlock()
		return
	}
	if p.waiter == nil {
		p.waiter = make(chan struct{})
	}
	waiter := p.waiter
	p.mu.Unlock()
	select {
	case <-waiter:
	case <-ctx.Done():
	}
}

// frame is the part of a JSON-RPC message the bookkeeping reads.
type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

// frameID is the id of a request (a frame with a method and an id) or, with
// response set, of a response (an id and no method). ok is false for
// anything else: a notification, a batch, a line that is not JSON.
// Numbers and strings never collide, and 1 and 1.0 are the same id.
func frameID(line []byte, response bool) (id string, ok bool) {
	var f frame
	if json.Unmarshal(line, &f) != nil || len(f.ID) == 0 || (f.Method == "") != response {
		return "", false
	}
	var v any
	if json.Unmarshal(f.ID, &v) != nil {
		return "", false
	}
	switch v := v.(type) {
	case float64:
		return "n" + strconv.FormatFloat(v, 'g', -1, 64), true
	case string:
		return "s" + v, true
	}
	return "", false
}

// lines splits a stream into newline-terminated lines across arbitrary
// chunks, handing each complete one to emit.
type lines struct {
	buf  []byte
	emit func(line []byte)
}

func (l *lines) write(b []byte) {
	l.buf = append(l.buf, b...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return
		}
		l.emit(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
}

// drainReader is stdin as the transport reads it: it records the requests
// passing through and, at EOF, waits for their responses before reporting it.
type drainReader struct {
	ctx   context.Context
	r     io.Reader
	calls *pending
	in    lines
}

func (d *drainReader) Read(p []byte) (int, error) {
	if d.in.emit == nil {
		d.in.emit = func(line []byte) {
			if id, ok := frameID(line, false); ok {
				d.calls.add(id)
			}
		}
	}
	n, err := d.r.Read(p)
	d.in.write(p[:n])
	if err == io.EOF {
		if n > 0 {
			// The SDK must get these bytes before anyone waits on it; the
			// next Read reports the EOF again.
			return n, nil
		}
		d.calls.drained(d.ctx)
	}
	return n, err
}

// responseWriter is stdout as the transport writes it: it marks a request
// answered when its response goes by.
type responseWriter struct {
	w     io.Writer
	calls *pending

	mu  sync.Mutex
	out lines
}

func (r *responseWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out.emit == nil {
		r.out.emit = func(line []byte) {
			if id, ok := frameID(line, true); ok {
				r.calls.done(id)
			}
		}
	}
	n, err := r.w.Write(p)
	if err == nil {
		r.out.write(p[:n])
	}
	return n, err
}

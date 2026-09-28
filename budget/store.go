package budget

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/nv3to/mcp-kit"
)

// Store keeps full logs as files in one directory, so that a server can
// return a bounded part of a command's output and still serve the rest. It
// holds a bounded number of logs and drops the oldest to make room. A Store
// is safe for concurrent use.
type Store struct {
	dir string
	max int

	mu     sync.Mutex
	logs   map[string]*Log
	order  []string // handles, oldest first
	closed bool
}

// NewStore returns a store that keeps at most max logs in dir, which it
// creates when missing. A max below one is invalid.
func NewStore(dir string, max int) (*Store, error) {
	if max < 1 {
		return nil, mcpkit.Errorf(mcpkit.Invalid, "a store keeps at least one log, not %d", max)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, mcpkit.Errorf(mcpkit.Internal, "log directory: %v", err)
	}
	return &Store{dir: dir, max: max, logs: map[string]*Log{}}, nil
}

// Create starts an empty log and returns it. When the store is full the
// oldest log is removed first; its handle is unknown from then on.
func (s *Store) Create() (*Log, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, mcpkit.Errorf(mcpkit.Conflict, "the log store is closed")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, mcpkit.Errorf(mcpkit.Internal, "log handle: %v", err)
	}
	handle := hex.EncodeToString(raw[:])
	for len(s.order) >= s.max {
		if err := s.evict(); err != nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(filepath.Join(s.dir, handle+".log"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, mcpkit.Errorf(mcpkit.Internal, "create log: %v", err)
	}
	l := &Log{handle: handle, file: file}
	s.logs[handle] = l
	s.order = append(s.order, handle)
	return l, nil
}

// Open returns the log that Create issued handle for. Any other handle is
// not_found, including an empty one and one shaped like a path: a handle is
// looked up among the issued ones and never becomes part of a file name.
func (s *Store) Open(handle string) (*Log, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.logs[handle]
	if !ok {
		return nil, mcpkit.Errorf(mcpkit.NotFound, "no log %q", handle)
	}
	return l, nil
}

// Close removes every log the store keeps and refuses further ones. The
// directory itself stays. Logs already handed out stop working.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var errs []error
	for len(s.order) > 0 {
		if err := s.evict(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// evict removes the oldest log. The caller holds s.mu.
func (s *Store) evict() error {
	l := s.logs[s.order[0]]
	delete(s.logs, l.handle)
	s.order = s.order[1:]
	return l.discard()
}

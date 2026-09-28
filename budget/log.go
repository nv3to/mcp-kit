package budget

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nv3to/mcp-kit"
)

// Log is one full log of a Store. It is the io.Writer a server hands to a
// command, and the thing a caller later pages and searches. Lines end at a
// newline; text written after the last newline is a line too. A Log is safe
// for concurrent use, so it can be read while the command still writes.
type Log struct {
	handle string

	mu      sync.Mutex
	file    *os.File // nil once the store dropped the log
	size    int64
	starts  []int64 // the offset of every line's first byte
	midline bool    // the last byte written was not a newline
	err     error
}

// Match is one line found by Search: its position and its text without the
// newline.
type Match struct {
	Line int
	Text string
}

// Handle returns the opaque name of the log, the one Store.Open takes. A
// server returns it to the caller next to the part of the output it kept.
func (l *Log) Handle() string { return l.handle }

// Lines returns how many lines the log holds, so a caller can be told how
// much there is to page through.
func (l *Log) Lines() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.starts)
}

// Write appends p to the log. It fails with not_found once the store has
// dropped the log.
func (l *Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return 0, l.dropped()
	}
	n, err := l.file.WriteAt(p, l.size)
	for i, b := range p[:n] {
		if !l.midline {
			l.starts = append(l.starts, l.size+int64(i))
		}
		l.midline = b != '\n'
	}
	l.size += int64(n)
	if err != nil {
		return n, mcpkit.Errorf(mcpkit.Internal, "write log: %v", err)
	}
	return n, nil
}

// Page returns whole lines starting at line from, never more than limit
// characters, newlines included. next is the position to continue from and
// end reports that the log has no line at next. A line that alone exceeds the
// limit is returned cut to it, and next still moves past the line, so
// following next always terminates. A from below one reads from the first
// line. When the log cannot be read, Page reports the end and Err says why.
func (l *Log) Page(from, limit int) (text string, next int, end bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if from < 1 {
		from = 1
	}
	if l.file == nil {
		l.fail(l.dropped())
		return "", from, true
	}
	if from > len(l.starts) {
		return "", from, true
	}
	limit = max(limit, 0)

	// limit characters never take more than limit*UTFMax bytes.
	start := l.starts[from-1]
	size := l.size - start
	if int64(limit) < size/utf8.UTFMax {
		size = int64(limit) * utf8.UTFMax
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(l.file, start, size), buf); err != nil {
		l.fail(mcpkit.Errorf(mcpkit.Internal, "read log: %v", err))
		return "", from, true
	}

	used, left := 0, limit
	for next = from; next <= len(l.starts); next++ {
		stop := l.size
		if next < len(l.starts) {
			stop = l.starts[next]
		}
		stop -= start
		if stop > size {
			break
		}
		n := utf8.RuneCount(buf[used:stop])
		if n > left {
			break
		}
		used, left = int(stop), left-n
	}
	if next == from {
		used = prefix(string(buf), limit)
		next++
	}
	return string(buf[:used]), next, next > len(l.starts)
}

// Search returns the lines that match pattern, a regular expression in the
// syntax of the standard regexp package, in the order of the log. It returns
// at most max matches and the true total, so a caller knows what it was not
// shown. A pattern that does not compile is invalid.
func (l *Log) Search(pattern string, max int) (matches []Match, total int, err error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, 0, mcpkit.Errorf(mcpkit.Invalid, "pattern: %v", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil, 0, l.dropped()
	}
	r := bufio.NewReader(io.NewSectionReader(l.file, 0, l.size))
	for line := 1; ; line++ {
		text, err := r.ReadString('\n')
		if text != "" {
			text = strings.TrimSuffix(text, "\n")
			if re.MatchString(text) {
				total++
				if len(matches) < max {
					matches = append(matches, Match{Line: line, Text: text})
				}
			}
		}
		if err == io.EOF {
			return matches, total, nil
		}
		if err != nil {
			return nil, 0, mcpkit.Errorf(mcpkit.Internal, "read log: %v", err)
		}
	}
}

// Err returns the first failure Page met, or nil. Page has no error result,
// so that paging reads as a loop over next; a loop that ended early shows
// here.
func (l *Log) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// discard closes and removes the file. Later calls on the log fail.
func (l *Log) discard() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	name := l.file.Name()
	err := l.file.Close()
	l.file = nil
	if rerr := os.Remove(name); err == nil {
		err = rerr
	}
	if err != nil {
		return mcpkit.Errorf(mcpkit.Internal, "remove log: %v", err)
	}
	return nil
}

func (l *Log) dropped() error {
	return mcpkit.Errorf(mcpkit.NotFound, "log %s is no longer kept", l.handle)
}

// fail records err as the log's error unless it has one. The caller holds l.mu.
func (l *Log) fail(err error) {
	if l.err == nil {
		l.err = err
	}
}

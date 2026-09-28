package budget

import (
	"fmt"
	"regexp"
	"strings"
)

// Step removes one kind of fluff from a text. Steps work on text already
// taken out of a log, so positions keep referring to the log as written.
type Step func(text string) string

// Clean applies the steps to text in the order given. Order matters: strip
// colour before collapsing, or two lines that differ only in colour stay two.
func Clean(text string, steps ...Step) string {
	for _, step := range steps {
		text = step(text)
	}
	return text
}

var escape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// StripColour removes the terminal control sequences (colour, cursor
// movement) a command writes for a human's terminal. They cost characters
// and carry nothing a caller reads.
func StripColour(text string) string {
	return escape.ReplaceAllString(text, "")
}

// ReplacePath returns the step that replaces the path prefix by with
// wherever prefix is a whole leading part of a path: "/home/u/src" is
// replaced in "/home/u/src/a.go" and left alone in "/home/u/src2/a.go". A
// long checkout or sandbox directory repeated on every line is the usual
// target. An empty prefix changes nothing.
func ReplacePath(prefix, with string) Step {
	prefix = strings.TrimRight(prefix, "/")
	return func(text string) string {
		if prefix == "" {
			return text
		}
		var b strings.Builder
		for {
			i := strings.Index(text, prefix)
			if i < 0 {
				break
			}
			end := i + len(prefix)
			b.WriteString(text[:i])
			if end < len(text) && inName(text[end]) {
				b.WriteString(prefix)
			} else {
				b.WriteString(with)
			}
			text = text[end:]
		}
		b.WriteString(text)
		return b.String()
	}
}

// inName reports whether c can continue a file name, which means the prefix
// before it ended in the middle of one.
func inName(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_', c == '-', c == '.', c >= 0x80:
		return true
	}
	return false
}

// probes bounds how many later occurrences of a line are tried as the start
// of a repeated block, which bounds the work on a log made of one line.
const probes = 32

// CollapseRepeats keeps the first copy of a block of lines that occurs more
// than once, directly after itself or further down, removes the other
// copies, and puts a line with the count under the copy it kept:
//
//	[the 3 lines above occur 4 times]
//
// A block that repeats directly after itself is taken at its shortest
// period; otherwise the longest block that occurs again is taken. A block
// never starts at a blank line.
func CollapseRepeats(text string) string {
	body := strings.TrimSuffix(text, "\n")
	lines := strings.Split(body, "\n")

	ids := make([]int, len(lines))
	known := map[string]int{}
	var at [][]int // the positions of every distinct line, ascending
	for i, line := range lines {
		id, ok := known[line]
		if !ok {
			id = len(at)
			known[line] = id
			at = append(at, nil)
		}
		ids[i] = id
		at[id] = append(at[id], i)
	}

	gone := make([]bool, len(lines))
	// extent returns how many lines from i equal the lines from j, i < j,
	// without the first block running into the second.
	extent := func(i, j int) int {
		n := 0
		for i+n < j && j+n < len(lines) && !gone[i+n] && !gone[j+n] && ids[i+n] == ids[j+n] {
			n++
		}
		return n
	}

	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if gone[i] {
			continue
		}
		later := at[ids[i]]
		for len(later) > 0 && later[0] <= i {
			later = later[1:]
		}
		at[ids[i]] = later

		size := 0
		if strings.TrimSpace(lines[i]) != "" {
			tried := 0
			for _, j := range later {
				if gone[j] {
					continue
				}
				n := extent(i, j)
				if n == j-i {
					size = n
					break
				}
				size = max(size, n)
				if tried++; tried == probes {
					break
				}
			}
		}
		if size == 0 {
			out = append(out, lines[i])
			continue
		}

		count, free := 1, i+size
		for _, j := range later {
			if j >= free && extent(i, j) >= size {
				for k := j; k < j+size; k++ {
					gone[k] = true
				}
				count, free = count+1, j+size
			}
		}
		out = append(out, lines[i:i+size]...)
		if size == 1 {
			out = append(out, fmt.Sprintf("[the line above occurs %d times]", count))
		} else {
			out = append(out, fmt.Sprintf("[the %d lines above occur %d times]", size, count))
		}
		i += size - 1
	}
	return strings.Join(out, "\n") + text[len(body):]
}

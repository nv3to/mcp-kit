package budget_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nv3to/mcp-kit"
	"github.com/nv3to/mcp-kit/budget"
)

// recorded reads a log from testdata. Under plz the data sits below the
// working directory at its path in the repository; run by hand it sits
// beside this file.
func recorded(t *testing.T, name string) string {
	t.Helper()
	dirs := []string{"testdata", filepath.Join("budget", "testdata")}
	if _, file, _, ok := runtime.Caller(0); ok {
		dirs = append(dirs, filepath.Join(filepath.Dir(file), "testdata"))
	}
	for _, dir := range dirs {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return string(b)
		}
	}
	t.Fatalf("cannot find %s in %v", name, dirs)
	return ""
}

// texts are the logs the properties are checked over: the recorded ones, one
// without a final newline, and one that is a single very long line.
func texts(t *testing.T) map[string]string {
	t.Helper()
	all := map[string]string{}
	all["build"] = recorded(t, "build.txt")
	all["gotest"] = recorded(t, "gotest.txt")
	all["unterminated"] = strings.TrimSuffix(all["build"], "\n")
	all["one line"] = strings.Repeat("é→日", 400)
	all["empty"] = ""
	return all
}

func newStore(t *testing.T, max int) (*budget.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "logs")
	s, err := budget.NewStore(dir, max)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func newLog(t *testing.T, s *budget.Store, text string) *budget.Log {
	t.Helper()
	l, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	// Written in small pieces, the way a command writes. The wrappers hide
	// the methods that would make the copy one single write.
	src := struct{ io.Reader }{strings.NewReader(text)}
	if _, err := io.CopyBuffer(struct{ io.Writer }{l}, src, make([]byte, 7)); err != nil {
		t.Fatal(err)
	}
	return l
}

func wantKind(t *testing.T, err error, kind mcpkit.Kind) {
	t.Helper()
	var e *mcpkit.Error
	if !errors.As(err, &e) || e.Kind != kind {
		t.Errorf("got error %v, want kind %s", err, kind)
	}
}

// longest returns the length in characters of the longest line of text, its
// newline included.
func longest(text string) int {
	n := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		n = max(n, utf8.RuneCountInString(line))
	}
	return n
}

// clip returns the first n characters of s.
func clip(s string, n int) string {
	r := []rune(s)
	return string(r[:min(n, len(r))])
}

func TestCutKeepsWholeLinesWithinTheCap(t *testing.T) {
	for name, text := range texts(t) {
		total := utf8.RuneCountInString(text)
		for limit := -1; limit <= total+1; limit++ {
			kept, omitted := budget.Cut(text, limit)
			got := utf8.RuneCountInString(kept)
			if got > max(limit, 0) {
				t.Fatalf("%s: cap %d: kept %d characters", name, limit, got)
			}
			if got+omitted != total {
				t.Fatalf("%s: cap %d: kept %d and omitted %d, the text has %d", name, limit, got, omitted, total)
			}
			if !strings.HasPrefix(text, kept) {
				t.Fatalf("%s: cap %d: kept %q is not the start of the text", name, limit, kept)
			}
			if !utf8.ValidString(kept) {
				t.Fatalf("%s: cap %d: kept %q splits a character", name, limit, kept)
			}
			first, _, _ := strings.Cut(text, "\n")
			whole := kept == text || strings.HasSuffix(kept, "\n")
			if !whole && limit > utf8.RuneCountInString(first) {
				t.Fatalf("%s: cap %d: kept %q ends inside a line", name, limit, kept)
			}
		}
	}
}

func TestCutInsideAFirstLineThatIsTooLong(t *testing.T) {
	kept, omitted := budget.Cut("日本語のログ\nnext\n", 4)
	if kept != "日本語の" || omitted != 8 {
		t.Errorf("got %q and %d omitted", kept, omitted)
	}
}

func TestPagingReproducesTheLog(t *testing.T) {
	s, _ := newStore(t, 8)
	for name, text := range texts(t) {
		l := newLog(t, s, text)
		var lines []string
		if text != "" {
			lines = strings.SplitAfter(text, "\n")
			if lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
		}
		if l.Lines() != len(lines) {
			t.Fatalf("%s: %d lines, want %d", name, l.Lines(), len(lines))
		}
		for limit := 1; limit <= longest(text)+3; limit++ {
			var got, want strings.Builder
			for _, line := range lines {
				want.WriteString(clip(line, limit))
			}
			from, end := 1, len(lines) == 0
			for !end {
				var page string
				var next int
				page, next, end = l.Page(from, limit)
				if n := utf8.RuneCountInString(page); n > limit {
					t.Fatalf("%s: cap %d: a page of %d characters", name, limit, n)
				}
				if next <= from {
					t.Fatalf("%s: cap %d: next is %d after %d", name, limit, next, from)
				}
				got.WriteString(page)
				from = next
			}
			if from != len(lines)+1 {
				t.Fatalf("%s: cap %d: paging ended at line %d of %d", name, limit, from, len(lines))
			}
			if got.String() != want.String() {
				t.Fatalf("%s: cap %d: pages give\n%q\nwant\n%q", name, limit, got.String(), want.String())
			}
			if limit >= longest(text) && got.String() != text {
				t.Fatalf("%s: cap %d: the pages do not add up to the log", name, limit)
			}
		}
		if err := l.Err(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestPageOfOneVeryLongLine(t *testing.T) {
	s, _ := newStore(t, 1)
	line := strings.Repeat("é→日", 100_000)
	l := newLog(t, s, line)
	text, next, end := l.Page(1, 50)
	if text != clip(line, 50) || next != 2 || !end {
		t.Errorf("got %d characters, next %d, end %v", utf8.RuneCountInString(text), next, end)
	}
}

func TestPageOutsideTheLog(t *testing.T) {
	s, _ := newStore(t, 1)
	l := newLog(t, s, "one\ntwo\n")
	if text, next, end := l.Page(0, 100); text != "one\ntwo\n" || next != 3 || !end {
		t.Errorf("from 0: got %q, next %d, end %v", text, next, end)
	}
	if text, next, end := l.Page(3, 100); text != "" || next != 3 || !end {
		t.Errorf("past the end: got %q, next %d, end %v", text, next, end)
	}
	if text, next, end := l.Page(1, 0); text != "" || next != 2 || end {
		t.Errorf("cap 0: got %q, next %d, end %v", text, next, end)
	}
}

func TestPageSeesWhatWasWrittenSince(t *testing.T) {
	s, _ := newStore(t, 1)
	l := newLog(t, s, "one\n")
	_, next, end := l.Page(1, 100)
	if next != 2 || !end {
		t.Fatalf("next %d, end %v", next, end)
	}
	if _, err := io.WriteString(l, "two\n"); err != nil {
		t.Fatal(err)
	}
	if text, next, end := l.Page(next, 100); text != "two\n" || next != 3 || !end {
		t.Errorf("got %q, next %d, end %v", text, next, end)
	}
}

func TestSearchBoundsTheMatchesAndCountsAll(t *testing.T) {
	s, _ := newStore(t, 1)
	text := recorded(t, "build.txt")
	l := newLog(t, s, text)

	matches, total, err := l.Search(`connection refused$`, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(matches) != 2 {
		t.Fatalf("got %d matches of %d, want 2 of 4", len(matches), total)
	}
	if matches[0].Line != 15 || matches[1].Line != 17 {
		t.Errorf("got lines %d and %d, want 15 and 17", matches[0].Line, matches[1].Line)
	}

	matches, total, err = l.Search(`é|→|✓`, 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 10 || len(matches) != total {
		t.Fatalf("got %d matches of %d, want 10 of 10", len(matches), total)
	}
	for _, m := range matches {
		page, next, _ := l.Page(m.Line, utf8.RuneCountInString(m.Text)+1)
		if page != m.Text+"\n" || next != m.Line+1 {
			t.Errorf("line %d: the page is %q, the match %q", m.Line, page, m.Text)
		}
	}

	if matches, total, err = l.Search(`warning`, 0); err != nil || len(matches) != 0 || total != 3 {
		t.Errorf("max 0: got %d matches of %d, %v", len(matches), total, err)
	}
	if matches, total, err = l.Search(`no such text`, 5); err != nil || len(matches) != 0 || total != 0 {
		t.Errorf("no match: got %d matches of %d, %v", len(matches), total, err)
	}
}

func TestSearchRefusesABadPattern(t *testing.T) {
	s, _ := newStore(t, 1)
	_, _, err := newLog(t, s, "text\n").Search(`(`, 5)
	wantKind(t, err, mcpkit.Invalid)
}

func TestOpenFindsOnlyIssuedHandles(t *testing.T) {
	s, dir := newStore(t, 2)
	l := newLog(t, s, "kept\n")

	found, err := s.Open(l.Handle())
	if err != nil {
		t.Fatal(err)
	}
	if text, _, _ := found.Page(1, 100); text != "kept\n" {
		t.Errorf("the opened log holds %q", text)
	}
	if strings.ContainsAny(l.Handle(), `/\.`) {
		t.Errorf("handle %q is shaped like a path", l.Handle())
	}

	// A file beside the store's directory, and the names that would reach
	// it or a log's own file if a handle were ever joined to a path.
	outside := filepath.Join(filepath.Dir(dir), "secret.log")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{
		"",
		"0123456789abcdef0123456789abcdef",
		"../secret",
		"../secret.log",
		outside,
		strings.TrimSuffix(outside, ".log"),
		"./" + l.Handle(),
		`..\secret`,
		l.Handle() + ".log",
		filepath.Join(dir, l.Handle()),
	} {
		got, err := s.Open(handle)
		if got != nil {
			t.Errorf("handle %q opened a log", handle)
		}
		wantKind(t, err, mcpkit.NotFound)
	}
}

func files(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestStoreEvictsTheOldest(t *testing.T) {
	s, dir := newStore(t, 2)
	first := newLog(t, s, "first\n")
	second := newLog(t, s, "second\n")
	third := newLog(t, s, "third\n")

	_, err := s.Open(first.Handle())
	wantKind(t, err, mcpkit.NotFound)
	for _, l := range []*budget.Log{second, third} {
		if _, err := s.Open(l.Handle()); err != nil {
			t.Errorf("a log within the bound is gone: %v", err)
		}
	}
	if n := files(t, dir); n != 2 {
		t.Errorf("the directory holds %d files, want 2", n)
	}

	// The evicted log, still held by its writer, fails instead of reading
	// a file that is gone.
	_, err = io.WriteString(first, "more\n")
	wantKind(t, err, mcpkit.NotFound)
	_, _, err = first.Search(`first`, 1)
	wantKind(t, err, mcpkit.NotFound)
	if text, _, end := first.Page(1, 100); text != "" || !end {
		t.Errorf("an evicted log paged %q, end %v", text, end)
	}
	wantKind(t, first.Err(), mcpkit.NotFound)
}

func TestCloseRemovesEverything(t *testing.T) {
	s, dir := newStore(t, 4)
	l := newLog(t, s, "one\n")
	newLog(t, s, "two\n")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := files(t, dir); n != 0 {
		t.Errorf("the directory holds %d files after Close", n)
	}
	_, err := s.Open(l.Handle())
	wantKind(t, err, mcpkit.NotFound)
	_, err = s.Create()
	wantKind(t, err, mcpkit.Conflict)
}

func TestNewStoreRefusesABoundBelowOne(t *testing.T) {
	_, err := budget.NewStore(t.TempDir(), 0)
	wantKind(t, err, mcpkit.Invalid)
}

func TestStripColour(t *testing.T) {
	text := budget.StripColour(recorded(t, "build.txt"))
	if strings.Contains(text, "\x1b") {
		t.Errorf("an escape is left in %q", text)
	}
	for _, want := range []string{
		"==> compiling /Users/dev/work/acme/cmd/acme\n",
		"warning: /Users/dev/work/acme/internal/cache/lru.go:41: field épaisseur is never read\n",
		"✓ internal/cache 12 tests\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is missing from %q", want, text)
		}
	}
}

func TestReplacePath(t *testing.T) {
	step := budget.ReplacePath("/Users/dev/work/acme/", ".")
	for _, c := range [][2]string{
		{"/Users/dev/work/acme/cmd/acme", "./cmd/acme"},
		{"make -C /Users/dev/work/acme test", "make -C . test"},
		{"in /Users/dev/work/acme", "in ."},
		{"/Users/dev/work/acme2/cmd", "/Users/dev/work/acme2/cmd"},
		{"/Users/dev/work/acme.old/cmd", "/Users/dev/work/acme.old/cmd"},
		{"a /Users/dev/work/acme/x b /Users/de", "a ./x b /Users/de"},
	} {
		if got := step(c[0]); got != c[1] {
			t.Errorf("%q became %q, want %q", c[0], got, c[1])
		}
	}
	if got := budget.ReplacePath("", ".")("/a/b"); got != "/a/b" {
		t.Errorf("an empty prefix changed the text to %q", got)
	}
}

func TestCollapseRepeatsInARecordedLog(t *testing.T) {
	text := recorded(t, "build.txt")
	got := budget.Clean(text, budget.StripColour, budget.ReplacePath("/Users/dev/work/acme", "."), budget.CollapseRepeats)
	want := `$ make -C . test
==> compiling ./cmd/acme
warning: ./internal/cache/lru.go:41: field épaisseur is never read
    41 |     épaisseur int
       |     ^
[the 3 lines above occur 3 times]
==> compiling ./cmd/acmectl
==> compiling ./cmd/acmed
==> testing ./internal/store
dial tcp 10.0.0.7:5432: connect: connection refused
  retrying in 2s → attempt failed
[the 2 lines above occur 3 times]
--- FAIL: TestOpen (6.01s)
    store_test.go:58: open: dial tcp 10.0.0.7:5432: connect: connection refused
✓ internal/cache 12 tests
✗ internal/store 1 of 9 tests failed
make: *** [test] Error 1
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestCollapseRepeats(t *testing.T) {
	for _, c := range [][2]string{
		{"", ""},
		{"a\nb\nc\n", "a\nb\nc\n"},
		{"a\na\na\na\n", "a\n[the line above occurs 4 times]\n"},
		{"a\nb\na\nb\na\nb\nend", "a\nb\n[the 2 lines above occur 3 times]\nend"},
		{"a\nx\na\ny\na", "a\n[the line above occurs 3 times]\nx\ny"},
		{"a\nb\nc\nx\na\nb\nc\ny", "a\nb\nc\n[the 3 lines above occur 2 times]\nx\ny"},
		{"x\n\ny\n\nz\n", "x\n\ny\n\nz\n"},
	} {
		if got := budget.CollapseRepeats(c[0]); got != c[1] {
			t.Errorf("%q became %q, want %q", c[0], got, c[1])
		}
	}
}

// The steps work on text taken out of a log: the log, its positions and what
// Search finds stay as written.
func TestCleaningLeavesPositionsAlone(t *testing.T) {
	s, _ := newStore(t, 1)
	l := newLog(t, s, recorded(t, "build.txt"))
	page, next, _ := l.Page(17, 60)
	if page != "dial tcp 10.0.0.7:5432: connect: connection refused\n" || next != 18 {
		t.Fatalf("got %q, next %d", page, next)
	}
	budget.Clean(page, budget.StripColour, budget.CollapseRepeats)
	if again, _, _ := l.Page(17, 60); again != page {
		t.Errorf("the log changed: %q", again)
	}
	if l.Lines() != 25 {
		t.Errorf("the log has %d lines, want 25", l.Lines())
	}
}

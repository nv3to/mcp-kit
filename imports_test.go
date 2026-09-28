package mcpkit_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const (
	module = "github.com/nv3to/mcp-kit"
	sdk    = "github.com/modelcontextprotocol/go-sdk/mcp"
)

// packages are the directories of the module's packages, the root first.
var packages = []string{".", "mcptest", "budget"}

// sourceDirs finds the directory of every package. Under plz the sources
// arrive as test data next to the working directory; run by hand they sit
// beside this file.
func sourceDirs(t *testing.T) []string {
	t.Helper()
	bases := []string{"."}
	if _, file, _, ok := runtime.Caller(0); ok {
		bases = append(bases, filepath.Dir(file))
	}
	for _, base := range bases {
		var dirs []string
		for _, pkg := range packages {
			dir := filepath.Join(base, pkg)
			if files, _ := filepath.Glob(filepath.Join(dir, "*.go")); len(files) > 0 {
				dirs = append(dirs, dir)
			}
		}
		if len(dirs) == len(packages) {
			return dirs
		}
	}
	t.Fatalf("cannot find the sources of %v from %v", packages, bases)
	return nil
}

// The library travels into every server built on it, so it brings the
// standard library, the SDK's mcp package and itself, and nothing else. Under
// plz the walk sees the library sources; run by hand it sees the test files too.
func TestImportsOnlyTheSDK(t *testing.T) {
	checked := 0
	for _, dir := range sourceDirs(t) {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			checked++
			for _, imp := range f.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: bad import %s", file, imp.Path.Value)
				}
				std := !strings.Contains(strings.SplitN(path, "/", 2)[0], ".")
				self := path == module || strings.HasPrefix(path, module+"/")
				if !std && !self && path != sdk {
					t.Errorf("%s imports %s: mcpkit may import only the standard library, %s and itself", file, path, sdk)
				}
			}
		}
	}
	if checked < 8 {
		t.Errorf("checked only %d source files; the walk is not seeing the package", checked)
	}
}

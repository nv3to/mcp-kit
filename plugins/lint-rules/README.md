# lint-rules

A [Please](https://please.build) plugin that runs linters as ordinary plz
tests, so `plz test //...` fails on unformatted Go, `go vet` findings and
pyflakes findings. It also fails on any source file that no linter covers.

- **`lint()`** goes in each BUILD file and checks that package's own files. It
  is hermetic and cached like any other test.
- **`repo_lint()`** goes once in the root BUILD file. It runs `go vet` over the
  module and the coverage guard.

Every tool is supplied by the consuming repo, so the plugin pins nothing and
adds no dependency.

## Install

In `plugins/BUILD`:

```python
plugin_repo(
    name = "lint",
    owner = "nv3to",
    plugin = "lint-rules",
    revision = "v0.1.0",
)
```

Vendored in-tree, as it is today, the equivalent is
`subrepo(name = "lint", path = "plugins/lint-rules", plugin = True)`.

In `.plzconfig`:

```ini
[Parse]
PreloadSubincludes = ///lint//build_defs:lint

[Plugin "lint"]
Target = //plugins:lint
GofmtTool = //third_party/go:toolchain|gofmt
GoTool = //third_party/go:toolchain|go
PyflakesTool = //tools:pyflakes   ; optional: a python_binary running pyflakes.api.main
```

## Use

Add this at the end of every BUILD file that owns `.go` or `.py` files:

```python
lint()                                  # this directory's *.go and *.py
lint(srcs = glob(["**/*.py"]))          # a package that owns sub-directories
```

Add this once, in the root BUILD file:

```python
repo_lint(
    name = "vet",
    packages = ["//cmd/app", "//internal/store"],   # every package that calls lint()
)
```

| Target | Fails when |
|---|---|
| `:lint_gofmt` | `gofmt -l` names a file (the diff is printed) |
| `:lint_pyflakes` | pyflakes reports anything |
| `//:vet` (repo_lint) | `go vet` reports anything, or a non-ignored `.go`/`.py` file outside `testdata/` is not in any listed package's `lint()` |

All of them carry the `lint` label, so `plz test //... --include lint` runs
lint alone.

## Config

| Key | Default | Meaning |
|---|---|---|
| `GofmtTool` | unset (skip) | gofmt to run |
| `GoTool` | unset (skip vet) | go, used for `go vet` |
| `PyflakesTool` | unset (skip) | pyflakes executable |
| `Label` | `lint` | label on every lint test |
| `Exclude` | none | repo-relative prefixes the coverage guard ignores (repeatable) |

## Limits

- `repo_lint()` runs locally and outside the sandbox. `go vet` needs the whole
  module and the Go module and build caches, which come from `HOME`, `GOPATH`,
  `GOMODCACHE` and `GOCACHE`.
- `repo_lint()` re-runs when a listed package's sources change. A brand-new
  package that no edit to a listed package accompanies is caught on the next
  run after one does, or with `plz test //:vet --rerun`.
- Unix only: the repo-wide script is POSIX `sh`.

## Licence

MIT. See [LICENSE](LICENSE).

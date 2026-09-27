# AGENTS.md — lint-rules

Instructions for AI coding agents and contributors. `CLAUDE.md` and
`GEMINI.md` are symlinks to this file. User documentation is in
[README.md](README.md).

## Layout

| Path | What |
|---|---|
| `.plzconfig` | `[PluginDefinition]` and the `[PluginConfig]` keys |
| `build_defs/lint.build_defs` | `lint()` and `repo_lint()` |
| `tools/repo_lint.sh` | the coverage guard and `go vet` (POSIX sh) |

## Rules

- **Nothing is pinned here.** Every tool comes from a config key that the
  consumer sets. Do not add a download, a toolchain or a dependency.
- **An unset tool key skips that linter.** It never fails.
- **`lint()` stays hermetic.** Only `repo_lint()` is local and unsandboxed.
- **No consumer names.** Nothing may refer to a particular repo.
- **Config keys and target names (`lint`, `lint_gofmt`, `lint_pyflakes`) are
  the public API.** Renaming one breaks every consumer.

## Testing a change

This plugin has no test suite of its own yet. Verify a change in a consuming
repo:

1. `plz test //... --include lint` is green.
2. Plant each defect and confirm that its gate fails, then revert:
   - a misformatted `.go` file fails `lint_gofmt`;
   - an unused Python import fails `lint_pyflakes`;
   - `fmt.Printf("%d", "x")` fails `//:vet`;
   - a new package without `lint()` fails `//:vet`.

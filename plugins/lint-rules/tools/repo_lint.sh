#!/bin/sh
# repo_lint: the repo-wide half of lint-rules, run by repo_lint() as a plz test.
#
#   repo_lint.sh [--go GO] [--exclude PREFIX]...
#
# 1. Coverage guard: every non-ignored *.go or *.py file (outside testdata/ and
#    the excludes) is among the files the lint() tests check. Those arrive as
#    this test's data ($DATA, repo-relative paths), so the check is exact.
# 2. `go vet` over every package of the module, when --go is given.
set -u

go=""
excludes=""
while [ $# -gt 0 ]; do
  case $1 in
    --go) go=$2; shift 2 ;;
    --exclude) excludes="$excludes $2"; shift 2 ;;
    *) echo "repo_lint: unknown argument $1" >&2; exit 2 ;;
  esac
done

work=$(mktemp -d) || exit 1
trap 'rm -rf "$work"' EXIT
# shellcheck disable=SC2086 # $DATA is a space-separated list of paths
printf '%s\n' ${DATA:-} | sort -u > "$work/linted"

# Tools arrive relative to the test directory; pin them before leaving it.
case $go in /* | "") ;; *) go=$PWD/$go ;; esac

# The test runs under <root>/plz-out/; the repo root is everything before it.
root=${PWD%%/plz-out/*}
if [ ! -f "$root/.plzconfig" ]; then
  echo "repo_lint: cannot find the repo root above $PWD" >&2
  exit 1
fi
cd "$root" || exit 1

fail=0

# Tracked plus untracked-but-not-ignored, so a new package is caught before it is added.
if ! git ls-files --cached --others --exclude-standard -- '*.go' '*.py' > "$work/tracked" 2>/dev/null || [ ! -s "$work/tracked" ]; then
  find . \( -name plz-out -o -name .git \) -prune -o \( -name '*.go' -o -name '*.py' \) -print |
    sed 's|^\./||' > "$work/tracked"
fi
grep -v -e '^testdata/' -e '/testdata/' "$work/tracked" | sort -u > "$work/wanted"

for f in $(comm -23 "$work/wanted" "$work/linted"); do
  skip=0
  for p in $excludes; do
    case "$f" in "${p%/}/"*) skip=1 ;; esac
  done
  [ $skip = 1 ] && continue
  echo "repo_lint: $f is not linted: its package must call lint() and be listed in repo_lint(packages = [...])"
  fail=1
done

# Vet exactly the linted Go directories. `./...` would walk plz-out, where
# other tests create and delete directories mid-walk.
if [ -n "$go" ] && [ -f go.mod ]; then
  dirs=$(grep '\.go$' "$work/linted" | sed -e 's|/[^/]*$||' -e 's|^[^/]*\.go$|.|' | sort -u | sed 's|^|./|')
  if [ -n "$dirs" ]; then
    # shellcheck disable=SC2086 # one argument per directory
    pkgs=$("$go" list -e -f '{{if or .GoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' $dirs) || exit 1
    # shellcheck disable=SC2086 # one argument per package
    [ -z "$pkgs" ] || "$go" vet $pkgs || fail=1
  fi
fi

exit $fail

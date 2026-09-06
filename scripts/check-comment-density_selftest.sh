#!/usr/bin/env bash
# Proves check-comment-density.sh catches each of its three checks on a
# seeded violation and names the right file(s)/line(s), and that it passes
# both a clean diff and a diff below the density floor. The lexer's own
# per-character comment/string state machine (comment_density.py) is
# exercised indirectly through these fixtures rather than unit-tested in
# isolation, matching this repo's existing selftest-over-fixture style for
# other scripts/*.sh checkers.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
checker="$root/scripts/check-comment-density.sh"

fixture=""
trap 'rm -rf "$fixture"' EXIT
fixture="$(mktemp -d)"

(
  cd "$fixture"
  git init -q
  git config user.email test@example.com
  git config user.name test
  mkdir -p app
  printf 'package app\n' >app/base.go
  git add -A
  git commit -q -m base
)
base_sha="$(git -C "$fixture" rev-parse HEAD)"

# reset_to_base + commit_scenario let each scenario branch from the same
# base commit instead of stacking on the previous scenario's changes, so
# every run_check call below diffs base_sha against a head that contains
# only that one scenario's seeded content.
reset_to_base() {
  git -C "$fixture" checkout -q -B work "$base_sha"
  git -C "$fixture" clean -qfdx
}

commit_scenario() {
  git -C "$fixture" add -A
  git -C "$fixture" commit -q -m scenario
  git -C "$fixture" rev-parse HEAD
}

run_check() {
  local head_sha="$1"
  COMMENT_DENSITY_CHECK_ROOT="$fixture" "$checker" "$base_sha" "$head_sha"
}

# Scenario 1: density violation -- 30+ added non-blank lines, comment ratio
# above 15%.
reset_to_base
{
  echo "package app"
  echo
  echo "func Dense() int {"
  for i in $(seq 1 25); do
    echo "	// comment line $i explaining nothing at all"
  done
  for i in $(seq 1 5); do
    echo "	x$i := $i"
  done
  echo "	return 1"
  echo "}"
} >"$fixture/app/dense.go"
head_sha="$(commit_scenario)"

if run_check "$head_sha" >/tmp/comment-density-selftest-density.log 2>&1; then
  echo "FAIL: checker passed against a seeded density violation" >&2
  cat /tmp/comment-density-selftest-density.log >&2
  exit 1
fi
if ! grep -qE 'app/dense\.go:[0-9]+: density [0-9.]+% > 15%' /tmp/comment-density-selftest-density.log; then
  echo "FAIL: checker did not name the file and ratio for the density violation" >&2
  cat /tmp/comment-density-selftest-density.log >&2
  exit 1
fi
echo "ok: checker fails and names the file+ratio on a seeded density violation"

# Scenario 2: block-length violation -- a 9-line consecutive comment run.
reset_to_base
{
  echo "package app"
  echo
  echo "func Block() int {"
  for i in $(seq 1 9); do
    echo "	// line $i of a long comment block"
  done
  echo "	return 1"
  echo "}"
} >"$fixture/app/block.go"
head_sha="$(commit_scenario)"

if run_check "$head_sha" >/tmp/comment-density-selftest-block.log 2>&1; then
  echo "FAIL: checker passed against a seeded 9-line comment block" >&2
  cat /tmp/comment-density-selftest-block.log >&2
  exit 1
fi
if ! grep -qE 'app/block\.go:4: block-length 9 > 8' /tmp/comment-density-selftest-block.log; then
  echo "FAIL: checker did not name the first line of the offending comment block" >&2
  cat /tmp/comment-density-selftest-block.log >&2
  exit 1
fi
echo "ok: checker fails and names the first line on a seeded block-length violation"

# Scenario 3: duplication -- the same normalized sentence in two added
# comments must fail and name both sites.
reset_to_base
{
  echo "package app"
  echo
  echo "// A non-zero exit is a fetch failure, not an evaluation failure."
  echo "func FetchOutput() error {"
  echo "	return nil"
  echo "}"
  echo
  echo "// A non-zero exit is a fetch failure, not an evaluation failure."
  echo "func FetchOther() error {"
  echo "	return nil"
  echo "}"
} >"$fixture/app/dup.go"
head_sha="$(commit_scenario)"

if run_check "$head_sha" >/tmp/comment-density-selftest-dup.log 2>&1; then
  echo "FAIL: checker passed against a seeded duplicated comment sentence" >&2
  cat /tmp/comment-density-selftest-dup.log >&2
  exit 1
fi
if ! grep -qE 'app/dup\.go:3: duplication 2 > 1' /tmp/comment-density-selftest-dup.log \
  || ! grep -qE 'app/dup\.go:8: duplication 2 > 1' /tmp/comment-density-selftest-dup.log; then
  echo "FAIL: checker did not name both sites of the duplicated sentence" >&2
  cat /tmp/comment-density-selftest-dup.log >&2
  exit 1
fi
echo "ok: checker fails and names both sites on a seeded duplicated sentence"

# Scenario 4: a 25-non-blank-line file at ~23% density passes -- below the
# 30-line floor, where the ratio is not meaningful.
reset_to_base
{
  echo "func Sparse() int {"
  for i in $(seq 1 5); do
    echo "	// note $i"
    echo "	x$i := $i"
  done
  for i in $(seq 6 15); do
    echo "	y$i := $i"
  done
  echo "	return 1"
} >"$fixture/app/sparse.go"
head_sha="$(commit_scenario)"

if ! run_check "$head_sha" >/tmp/comment-density-selftest-floor.log 2>&1; then
  echo "FAIL: checker failed against a file below the density floor" >&2
  cat /tmp/comment-density-selftest-floor.log >&2
  exit 1
fi
echo "ok: checker passes a dense-but-short file below the 30-line floor"

# Scenario 5: a clean, ordinary change passes outright.
reset_to_base
{
  echo "package app"
  echo
  echo "// Clean adds two ints together."
  echo "func Clean(a, b int) int {"
  echo "	return a + b"
  echo "}"
} >"$fixture/app/clean.go"
head_sha="$(commit_scenario)"

if ! run_check "$head_sha" >/tmp/comment-density-selftest-clean.log 2>&1; then
  echo "FAIL: checker failed against a clean diff" >&2
  cat /tmp/comment-density-selftest-clean.log >&2
  exit 1
fi
echo "ok: checker passes a clean diff"

# Scenario 6: a stale/advanced base must not misattribute a pre-existing,
# PR-untouched file's content as "added". The fork point's shared.go carries
# a 20-line comment block written long before this change; the stale base
# (standing in for a moving github.event.pull_request.base.sha) rewrites
# that same file into something sharing almost no lines with it, unrelated
# to this PR; this PR's own head forks from the same point and never
# touches shared.go at all. A plain two-dot diff(stale_base, pr_head) can't
# tell any of that apart from base's and head's content alone: since almost
# nothing lines up between base's rewrite and head's untouched original, it
# would score most of that 20-line block as newly "added" by this PR. The
# fix (diffing against merge-base(base, head), i.e. the fork point) must
# see that shared.go is identical between the fork point and head, and
# skip it.
git -C "$fixture" checkout -q -B fork-point "$base_sha"
git -C "$fixture" clean -qfdx
{
  for i in $(seq 1 20); do
    echo "// pre-existing doc line $i, written long before this change"
  done
  echo "package app"
  echo "func Old() int { return 1 }"
} >"$fixture/app/shared.go"
git -C "$fixture" add -A
git -C "$fixture" commit -q -m "fork point: pre-existing comment-heavy file"
fork_sha="$(git -C "$fixture" rev-parse HEAD)"

git -C "$fixture" checkout -q -B stale-main "$fork_sha"
{
  echo "package app"
  echo "func New() int { return 2 }"
} >"$fixture/app/shared.go"
git -C "$fixture" add -A
git -C "$fixture" commit -q -m "main: unrelated rewrite of shared.go"
stale_base_sha="$(git -C "$fixture" rev-parse HEAD)"

git -C "$fixture" checkout -q -B pr-head "$fork_sha"
{
  echo "package app"
  echo
  echo "// Feature adds one thing."
  echo "func Feature() int {"
  echo "	return 3"
  echo "}"
} >"$fixture/app/feature.go"
git -C "$fixture" add -A
git -C "$fixture" commit -q -m "pr: add feature.go"
pr_head_sha="$(git -C "$fixture" rev-parse HEAD)"

if ! COMMENT_DENSITY_CHECK_ROOT="$fixture" "$checker" "$stale_base_sha" "$pr_head_sha" \
  >/tmp/comment-density-selftest-stale-base.log 2>&1; then
  echo "FAIL: checker misattributed a stale base's unrelated rewrite of an untouched file to this PR's head" >&2
  cat /tmp/comment-density-selftest-stale-base.log >&2
  exit 1
fi
echo "ok: checker ignores a pre-existing untouched file's content despite a stale/advanced base"

# Scenario 7: a comment inside a TypeScript template-literal's ${...}
# interpolation is executable code, not inert string content, and must
# still be scored.
reset_to_base
{
  echo "export function render(): string {"
  echo "  return \`prefix \${"
  for i in $(seq 1 9); do
    echo "    // interpolated line $i of a long comment block"
  done
  echo "    compute()"
  echo "  } suffix\`;"
  echo "}"
} >"$fixture/app/template.ts"
head_sha="$(commit_scenario)"

if run_check "$head_sha" >/tmp/comment-density-selftest-template.log 2>&1; then
  echo "FAIL: checker missed a comment block inside a template-literal interpolation" >&2
  cat /tmp/comment-density-selftest-template.log >&2
  exit 1
fi
if ! grep -qE 'app/template\.ts:[0-9]+: block-length 9 > 8' /tmp/comment-density-selftest-template.log; then
  echo "FAIL: checker did not name the block-length violation inside the template interpolation" >&2
  cat /tmp/comment-density-selftest-template.log >&2
  exit 1
fi
echo "ok: checker scores a comment block inside a TypeScript template-literal interpolation"

# Scenario 8: a Go raw string holding literal ${...} text (e.g. an embedded
# shell script) must stay inert string content -- Go raw strings have no
# template-interpolation syntax, unlike TypeScript's backtick, and treating
# the ${...} as executable code would also make the embedded "// comment"
# in the script text below register as a real Go line comment.
reset_to_base
{
  echo "package app"
  echo
  echo "func Script() string {"
  echo "	return \`#!/usr/bin/env bash"
  for i in $(seq 1 9); do
    echo "echo \"\${VAR_$i} // not a real comment\""
  done
  echo "\`"
  echo "}"
} >"$fixture/app/rawstring.go"
head_sha="$(commit_scenario)"

if ! run_check "$head_sha" >/tmp/comment-density-selftest-rawstring.log 2>&1; then
  echo "FAIL: checker misread a Go raw string's literal \${...} text as template interpolation" >&2
  cat /tmp/comment-density-selftest-rawstring.log >&2
  exit 1
fi
echo "ok: checker treats a Go raw string's literal \${...} text as inert, not a template interpolation"

#!/usr/bin/env bash
# check-all.sh has no root-override env var like the other checkers here:
# it is inherently whole-repo scoped (go vet/go test on real modules, the
# real check-*.sh scripts). So this exercises it against a disposable
# worktree of the repository's own HEAD rather than a synthetic fixture.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

worktree=""
cleanup() {
  if [ -n "$worktree" ]; then
    git -C "$root" worktree remove --force "$worktree" >/dev/null 2>&1 || true
    rm -rf "$worktree"
  fi
}
trap cleanup EXIT

# A worktree under the system tmpdir breaks this environment's `go build`
# VCS stamping (it collides with Go's own GOTMPDIR default), so this uses a
# sibling of the repository root instead, matching how this repository's
# own development worktrees are already laid out.
worktree="$(mktemp -d "$(dirname "$root")/check-all-selftest.XXXXXX")"
rmdir "$worktree"
git -C "$root" worktree add --detach -q "$worktree" HEAD
base_sha="$(git -C "$root" rev-parse HEAD)"

run_check() {
  (cd "$worktree" && ./scripts/check-all.sh "$base_sha")
}

if ! run_check >/tmp/check-all-selftest-clean.log 2>&1; then
  echo "FAIL: check-all.sh failed against a clean worktree" >&2
  cat /tmp/check-all-selftest-clean.log >&2
  exit 1
fi
if ! grep -q "check-all.sh: all checks passed" /tmp/check-all-selftest-clean.log; then
  echo "FAIL: check-all.sh did not report success on a clean worktree" >&2
  cat /tmp/check-all-selftest-clean.log >&2
  exit 1
fi
echo "ok: check-all.sh exits 0 and reports success on a clean worktree"

# Seed a comment-density violation (block-length above the checker's
# threshold of 8) in a real commit, so check-comment-density.sh's
# base/head diff actually sees it.
cat >>"$worktree/app/internal/traceid/traceid.go" <<'EOF'

// comment line 1 of a seeded density violation
// comment line 2 of a seeded density violation
// comment line 3 of a seeded density violation
// comment line 4 of a seeded density violation
// comment line 5 of a seeded density violation
// comment line 6 of a seeded density violation
// comment line 7 of a seeded density violation
// comment line 8 of a seeded density violation
// comment line 9 of a seeded density violation
func seededDensityViolation() int { return 0 }
EOF
git -C "$worktree" -c user.email=test@example.com -c user.name=test \
  commit -q -am "seed a comment-density violation for check-all_selftest.sh"

if run_check >/tmp/check-all-selftest-dirty.log 2>&1; then
  echo "FAIL: check-all.sh passed despite a seeded comment-density violation" >&2
  cat /tmp/check-all-selftest-dirty.log >&2
  exit 1
fi
if ! grep -q "FAILED: scripts/check-comment-density.sh" /tmp/check-all-selftest-dirty.log; then
  echo "FAIL: check-all.sh did not name check-comment-density.sh as the failing step" >&2
  cat /tmp/check-all-selftest-dirty.log >&2
  exit 1
fi
if grep -q "check-instruction-orphans.sh" /tmp/check-all-selftest-dirty.log; then
  echo "FAIL: check-all.sh ran a later step after the comment-density failure" >&2
  cat /tmp/check-all-selftest-dirty.log >&2
  exit 1
fi
echo "ok: check-all.sh stops at the first failing check and names it"

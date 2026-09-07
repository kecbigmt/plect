#!/usr/bin/env bash
# Proves verify-release-tag.sh actually catches each rejected case before
# it is trusted as the release workflow's gate: a non-SemVer tag, a
# lightweight tag on a real push, and a tag commit not reachable from main
# must all fail, while a valid push tag and a workflow_dispatch rehearsal
# (whose tag names a version, not necessarily an existing git ref) must
# both pass.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
checker="$root/scripts/verify-release-tag.sh"

repo=""
trap 'rm -rf "$repo"' EXIT
repo=$(mktemp -d)
cd "$repo"
git init -q -b main
git config user.email test@example.com
git config user.name test

echo one >file
git add file
git commit -q -m "main commit"
git tag v1.0.0 -m "release v1.0.0" # annotated, on main
git tag v1.0.1                     # lightweight, on main

# A commit main never merges: reachability must fail against it, unlike
# every other check above, which never depends on HEAD's position.
git checkout -qb off-branch
echo two >file
git commit -q -am "off-main commit"
git tag v9.9.9 -m "unreachable release" # annotated, but off main

expect_pass() {
  local desc="$1"
  shift
  if ! out=$("$checker" "$@" 2>&1); then
    echo "FAIL: $desc — expected pass, got: $out" >&2
    exit 1
  fi
  echo "ok: $desc"
}

expect_fail() {
  local desc="$1"
  local want_substr="$2"
  shift 2
  if out=$("$checker" "$@" 2>&1); then
    echo "FAIL: $desc — expected failure, checker passed: $out" >&2
    exit 1
  fi
  if ! grep -qF "$want_substr" <<<"$out"; then
    echo "FAIL: $desc — error did not mention '$want_substr': $out" >&2
    exit 1
  fi
  echo "ok: $desc"
}

git checkout -q main
expect_pass "valid annotated tag on push, reachable from main" v1.0.0 push main
expect_pass "workflow_dispatch rehearsal accepts a tag with no matching git ref" v2.0.0 workflow_dispatch main
expect_fail "non-SemVer tag" "not a strict SemVer tag" not-semver push main
expect_fail "lightweight tag on a real push" "not an annotated tag object" v1.0.1 push main
expect_fail "unknown event name" "unknown event" v1.0.0 merge_group main

git checkout -q off-branch
expect_fail "annotated tag whose commit main never merged" "not reachable" v9.9.9 push main

echo "verify-release-tag_selftest: all cases passed"

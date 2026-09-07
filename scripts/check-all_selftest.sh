#!/usr/bin/env bash
# Exercises check-all.sh's own sequencing and failure-naming, not the
# sub-checks' logic (each already has its own *_selftest.sh): a small
# synthetic fixture, scanned via CHECK_ALL_ROOT, keeps this fast instead of
# re-running the real repository's own go vet/go test through it.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
checker="$root/scripts/check-all.sh"
go_version="$(go env GOVERSION | sed 's/^go//')"

fixture=""
trap 'rm -rf "$fixture"' EXIT
fixture="$(mktemp -d)"

mkdir -p "$fixture/app" "$fixture/contracts/state" \
  "$fixture/.claude/skills" "$fixture/.agents/skills"

cat > "$fixture/app/go.mod" <<EOF
module app

go $go_version
EOF
cat > "$fixture/app/main.go" <<'EOF'
package main

func main() {}
EOF

cat > "$fixture/contracts/state/go.mod" <<EOF
module contracts/state

go $go_version
EOF
cat > "$fixture/contracts/state/state.go" <<'EOF'
package state
EOF

printf 'placeholder\n' > "$fixture/CLAUDE.md"
ln -s "CLAUDE.md" "$fixture/AGENTS.md"

(
  cd "$fixture"
  git init -q
  git config user.email test@example.com
  git config user.name test
  git add -A
  git commit -q -m base
)
base_sha="$(git -C "$fixture" rev-parse HEAD)"

run_check() {
  CHECK_ALL_ROOT="$fixture" "$checker" "$base_sha"
}

if ! run_check >/tmp/check-all-selftest-clean.log 2>&1; then
  echo "FAIL: check-all.sh failed against a clean fixture" >&2
  cat /tmp/check-all-selftest-clean.log >&2
  exit 1
fi
if ! grep -q "check-all.sh: all checks passed" /tmp/check-all-selftest-clean.log; then
  echo "FAIL: check-all.sh did not report success on a clean fixture" >&2
  cat /tmp/check-all-selftest-clean.log >&2
  exit 1
fi
echo "ok: check-all.sh exits 0 and reports success on a clean fixture"

# Seed a comment-density violation (block-length above the checker's
# threshold of 8) in a real commit, so check-comment-density.sh's
# base/head diff actually sees it.
cat >>"$fixture/app/main.go" <<'EOF'

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
git -C "$fixture" -c user.email=test@example.com -c user.name=test \
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

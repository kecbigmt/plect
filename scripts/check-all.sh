#!/usr/bin/env bash
# Local equivalent of every check ci.yml and agent-config-ci.yml run before
# a PR can merge: gofmt, go vet/go test on the modules AGENTS.md documents,
# and each scripts/check-*.sh those two workflows invoke, with the same
# arguments they pass. It only invokes the existing scripts — duplicating
# any one check's logic here would let this and CI drift apart.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

base_ref="${1:-}"
if [ -z "$base_ref" ]; then
  base_ref="$(git merge-base HEAD origin/main)"
fi

fail() {
  echo "check-all.sh: FAILED: $1" >&2
  exit 1
}

step() {
  local name="$1"
  shift
  echo "==> $name"
  if ! "$@"; then
    fail "$name"
  fi
}

echo "==> gofmt -l"
unformatted="$(gofmt -l $(find . -name '*.go' -not -path './app/internal/webui/*'))" || fail "gofmt -l"
if [ -n "$unformatted" ]; then
  echo "$unformatted"
  fail "gofmt -l"
fi

step "go vet ./app/..." bash -c 'cd app && go vet ./...'
step "go test ./app/..." bash -c 'cd app && go test ./...'
step "contracts/state go test ./..." bash -c 'cd contracts/state && go test ./...'

step "scripts/check-provider-boundary.sh" ./scripts/check-provider-boundary.sh
step "scripts/check-sqlite-driver-boundary.sh" ./scripts/check-sqlite-driver-boundary.sh
step "scripts/check-comment-references.sh" ./scripts/check-comment-references.sh
step "scripts/check-comment-density.sh $base_ref HEAD" ./scripts/check-comment-density.sh "$base_ref" HEAD
step "scripts/check-instruction-orphans.sh" ./scripts/check-instruction-orphans.sh
step "scripts/check-agent-config.sh" ./scripts/check-agent-config.sh

echo "check-all.sh: all checks passed"

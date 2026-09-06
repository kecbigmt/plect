#!/usr/bin/env bash
# Enforces the comment-shape rules CLAUDE.md's Comments section states
# (density, block length, no duplicated rationale), scoped to one PR's
# added/modified lines rather than whole files: a worker should hit this
# before opening the PR, on exactly the lines the PR is responsible for,
# not on pre-existing comments elsewhere in a touched file.
#
# The check logic itself lives in comment_density.py: language lexing (Go,
# TypeScript, SQL, TOML each have their own comment/string syntax) and
# multi-file sentence-duplication tracking are naturally a small stateful
# program, not a shell/awk one-liner, and this repo already keeps
# scripts/provider-vocab.py as a stdlib-only Python helper alongside its
# bash wrapper for the same reason.
set -euo pipefail

root="${COMMENT_DENSITY_CHECK_ROOT:-$(pwd)}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ "$#" -ne 2 ]; then
  echo "usage: check-comment-density.sh <base-ref> <head-ref>" >&2
  exit 2
fi

python3 "$script_dir/comment_density.py" "$root" "$1" "$2"

#!/usr/bin/env bash
# Verifies a release tag candidate before the release workflow's build
# matrix starts: strict SemVer, an annotated tag object on a real push (a
# workflow_dispatch rehearsal names a version string that may not exist as
# a tag at all), and that HEAD is reachable from main. Run from within the
# checked-out repository, with main-ref already fetched.
set -euo pipefail

usage() {
  echo "usage: $0 <tag> <event> <main-ref>" >&2
  echo "  event: push | workflow_dispatch" >&2
  exit 2
}

[ $# -eq 3 ] || usage
tag="$1"
event="$2"
main_ref="$3"

case "$event" in
  push | workflow_dispatch) ;;
  *)
    echo "verify-release-tag: unknown event '$event' (want push or workflow_dispatch)" >&2
    exit 1
    ;;
esac

if ! [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "verify-release-tag: '$tag' is not a strict SemVer tag (vMAJOR.MINOR.PATCH)" >&2
  exit 1
fi

if [ "$event" = push ]; then
  if [ "$(git cat-file -t "$tag" 2>/dev/null || true)" != tag ]; then
    echo "verify-release-tag: '$tag' is not an annotated tag object" >&2
    exit 1
  fi
fi

if ! git merge-base --is-ancestor HEAD "$main_ref"; then
  echo "verify-release-tag: HEAD is not reachable from '$main_ref'" >&2
  exit 1
fi

echo "verify-release-tag: '$tag' OK ($event, reachable from $main_ref)"

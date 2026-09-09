#!/usr/bin/env bash
# Extracts thread_workspace's setup script verbatim from the TOML and runs
# it directly, so this test exercises the exact shell the plugin ships
# rather than a hand-copied stand-in that could drift from it.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
toml="$here/../config/workspaces/thread_workspace.toml"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

q="'''"
setup_header_line=$(grep -n '^\[thread_workspace\.setup\]$' "$toml" | head -n 1 | cut -d: -f1)
open_line=$(awk -v start="$setup_header_line" -v q="$q" 'NR>=start && $0=="script = " q {print NR; exit}' "$toml")
close_line=$(awk -v start="$((open_line + 1))" -v q="$q" 'NR>=start && $0==q {print NR; exit}' "$toml")
script="$tmp/setup.sh"
sed -n "$((open_line + 1)),$((close_line - 1))p" "$toml" > "$script"

fail=0
check() {
  local label="$1" want="$2" got="$3"
  if [ "$got" != "$want" ]; then
    echo "FAIL $label: got '$got', want '$want'" >&2
    fail=1
  else
    echo "ok   $label"
  fi
}

run_setup() {
  local permalink="$1" session_name="$2"
  permalink="$permalink" session_name="$session_name" workspace_dirs_root="$tmp/root" \
    sh "$script"
}

root_permalink="https://example.slack.com/archives/C0BUUGNP3SL/p1788924553263429"
reply_permalink="https://example.slack.com/archives/C0BUUGNP3SL/p1788924553263429?thread_ts=1788924553.263429&cid=C0BUUGNP3SL"

out=$(run_setup "$root_permalink" "")
check "root permalink form: channel_id" "C0BUUGNP3SL" "$(jq -r '.channel_id' <<<"$out")"
check "root permalink form: thread_ts" "1788924553.263429" "$(jq -r '.thread_ts' <<<"$out")"

out=$(run_setup "$reply_permalink" "")
check "reply permalink form: channel_id" "C0BUUGNP3SL" "$(jq -r '.channel_id' <<<"$out")"
check "reply permalink form: thread_ts" "1788924553.263429" "$(jq -r '.thread_ts' <<<"$out")"

# A session name always carries a trailing "+<workflow-id>" (every session
# name in this codebase does), which is exactly what corrupted thread_ts
# when this script recovered it by splitting session_name instead of
# re-parsing permalink. session_name is passed here anyway, unused, to pin
# that the fix no longer depends on its shape at all.
out=$(run_setup "$root_permalink" "slack/C0BUUGNP3SL-1788924553263429+ops_chat")
check "a workflow-suffixed session name does not corrupt thread_ts" \
  "1788924553.263429" "$(jq -r '.thread_ts' <<<"$out")"

[ "$fail" -eq 0 ] || { echo "thread_workspace selftest failed" >&2; exit 1; }
echo "thread_workspace selftest passed"

#!/usr/bin/env bash
# Proves two properties across the worker's real process boundary — a
# stubbed `codex` binary and a background worker process, not a unit test
# of publish_reply in isolation — because both regressions this guards
# live at that boundary: a shell capture that loses bytes, and a restart
# that resets state a real restart must not.
#
#   1. A turn's captured reply reaches plect.message byte for byte,
#      trailing newlines included.
#   2. message_id keeps advancing across a codex-agent-activity reset —
#      the same reset exec_runtime's setup calls on every launch, resumes
#      included — rather than reusing an id an earlier turn already
#      published.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
worker="$here/codex-exec-worker"
activity="$here/codex-agent-activity"
tmp="$(mktemp -d)"

fail=0
worker_pid=""
cleanup() {
  [ -n "$worker_pid" ] && kill "$worker_pid" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

bin_dir="$tmp/bin"
mkdir -p "$bin_dir"

# The stub copies whatever currently sits at $STUB_REPLY_FILE to
# --output-last-message, re-reading that path fresh on every invocation —
# not its own inherited env at fork time — since the worker (and this stub
# under it) is a long-lived background process started once, before this
# test ever queues a turn; a value merely exported later in this script
# would never reach a process forked before that export ran. Reports a
# thread_id on a fresh call only, real enough to exercise the worker's own
# thread_id bookkeeping without needing a real codex binary.
cat > "$bin_dir/codex" <<'EOF'
#!/usr/bin/env bash
out=""
resume=false
for i in "$@"; do
  case "$prev" in
    --output-last-message) out="$i" ;;
  esac
  [ "$i" = "resume" ] && resume=true
  prev="$i"
done
if [ "$resume" != true ]; then
  echo '{"type":"thread.started","thread_id":"stub-thread-1"}'
fi
[ -n "$out" ] && cp "$STUB_REPLY_FILE" "$out"
exit 0
EOF
chmod +x "$bin_dir/codex"

cat > "$bin_dir/plect" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PLECT_CALLS"
EOF
chmod +x "$bin_dir/plect"

export PATH="$bin_dir:$PATH"
export PLECT_CALLS="$tmp/calls"
export PLECT_SESSION_NAME="selftest/worker-1"
export PLECT_AGENT_ACTIVITY_BIN="$activity"
export CODEX_PUBLISH_MESSAGE=true
export XDG_STATE_HOME="$tmp/xdg-state"
# The path itself, not its content, is what the worker's environment needs
# to carry — fixed and exported before the worker forks, then each turn
# below just overwrites the file at that path.
export STUB_REPLY_FILE="$tmp/next-reply.txt"
: > "$PLECT_CALLS"
"$activity" reset "$PLECT_SESSION_NAME"

queue_dir="$tmp/queue"
state_dir="$tmp/state"
mkdir -p "$queue_dir"

"$worker" "$queue_dir" "$state_dir" &
worker_pid=$!

wait_for_drain() {
  local file="$1"
  for _ in $(seq 1 100); do
    [ -e "$file" ] || return 0
    sleep 0.1
  done
  return 1
}

# extract_publish isolates the "event publish ..." call from a turn's other
# calls (working/waiting activity, tick) in $PLECT_CALLS: the publish call's
# own body can embed newlines, so it is the one call here that cannot be
# read one physical line at a time.
extract_publish() {
  awk '
    /^event publish/ { capturing=1 }
    capturing && !/^event publish/ && (/^state set-message/ || /^tick/) { exit }
    capturing { print }
  ' "$PLECT_CALLS"
}

# Turn 1: a reply ending in two trailing newlines must survive the worker's
# own file capture (mktemp -> codex -> jq --rawfile) byte for byte, not just
# whatever a $(cat ...) command substitution would have left after stripping
# them.
: > "$PLECT_CALLS"
printf 'paragraph one\n\n' > "$STUB_REPLY_FILE"
echo '{"type":"user_message","text":"turn one"}' > "$queue_dir/t1.json"
wait_for_drain "$queue_dir/t1.json" || { echo "FAIL turn 1 was never drained from the queue" >&2; fail=1; }

got="$(extract_publish)"
want='event publish selftest/worker-1 --type plect.message --summary paragraph one --body paragraph one

 --meta message_id=selftest/worker-1/reply-0 --meta message_id_origin=synthetic --meta role=assistant --meta source=codex'
if [ "$got" = "$want" ]; then
  echo "ok   a trailing-newline reply survives the worker's own capture byte for byte"
else
  printf 'FAIL trailing-newline capture: got %q, want %q\n' "$got" "$want" >&2
  fail=1
fi

# Simulate exec_runtime's setup calling reset on a restart — before the
# session's second turn, the way a real relaunch (resume included) would.
"$activity" reset "$PLECT_SESSION_NAME"

# Turn 2 (resume, since the stub already reported a thread_id): message_id
# must advance to reply-1, not reuse reply-0 — reset must not have rewound
# the counter this runtime's every message_id depends on.
: > "$PLECT_CALLS"
printf 'turn two reply' > "$STUB_REPLY_FILE"
echo '{"type":"user_message","text":"turn two"}' > "$queue_dir/t2.json"
wait_for_drain "$queue_dir/t2.json" || { echo "FAIL turn 2 was never drained from the queue" >&2; fail=1; }

got="$(extract_publish)"
case "$got" in
  *"message_id=selftest/worker-1/reply-1"*)
    echo "ok   message_id keeps advancing across a reset between turns" ;;
  *)
    printf 'FAIL a reset between turns must not reuse a message_id, got: %s\n' "$got" >&2
    fail=1 ;;
esac

kill "$worker_pid" 2>/dev/null || true
wait "$worker_pid" 2>/dev/null || true
worker_pid=""

[ "$fail" -eq 0 ] || { echo "codex-exec-worker selftest failed" >&2; exit 1; }
echo "codex-exec-worker selftest passed"

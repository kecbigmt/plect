#!/usr/bin/env bash
# Proves this plugin's activity probe reports evidence for a codex_exec
# turn even when the turn-boundary hooks alone would not: a single
# long-running `codex exec` call touches no hook until it ends, and its
# stdout never reaches the pane (it is redirected into the turn's own log
# file), so the pane fingerprint that covers this gap for an interactive
# agent (see plugins/tmux's pane.toml) cannot cover it here.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
activity="$here/codex-agent-activity"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export XDG_STATE_HOME="$tmp/xdg-state"
export PLECT_SESSION_NAME="selftest/session-1"
session="$PLECT_SESSION_NAME"
state_dir="$tmp/state"
mkdir -p "$state_dir/log"

bin_dir="$tmp/bin"
mkdir -p "$bin_dir"
cat > "$bin_dir/plect" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PLECT_CALLS"
EOF
chmod +x "$bin_dir/plect"
export PLECT_CALLS="$tmp/calls"
export PATH="$bin_dir:$PATH"

# noplect_path simulates plect being unreachable without filtering PATH
# down to a shortlist of its own: a stub named `plect` ahead of the real
# PATH always wins the lookup and fails loud, so bash/jq/coreutils stay
# reachable exactly as they are on the real PATH -- unlike excluding every
# directory that happens to contain a real `plect`, which on a host that
# ships coreutils and plect from the same directory would take out bash too.
noplect_bin_dir="$tmp/bin-noplect"
mkdir -p "$noplect_bin_dir"
cat > "$noplect_bin_dir/plect" <<'EOF'
#!/usr/bin/env bash
echo "plect: simulated unreachable (selftest)" >&2
exit 127
EOF
chmod +x "$noplect_bin_dir/plect"
noplect_path="$noplect_bin_dir:$PATH"

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

check_nonempty() {
  local label="$1" got="$2"
  if [ -z "$got" ]; then
    echo "FAIL $label: got empty output" >&2
    fail=1
  else
    echo "ok   $label"
  fi
}

"$activity" reset "$session"

# A fresh session with no turn yet reports no evidence — "no basis to judge"
# is not the same as "stalled".
out="$("$activity" probe "$session" "$state_dir")"
check "no evidence before any turn" "" "$out"

# A turn starting names its own log file before it writes anything to it —
# that name alone is evidence a turn began, with no hook call involved.
log_file="$state_dir/log/1000000000.jsonl"
: > "$log_file"
fp_start="$("$activity" probe "$session" "$state_dir" | jq -r .fingerprint)"
check_nonempty "turn start is evidence via its log file" "$fp_start"

# The same turn growing its log file — still no hook boundary crossed — is
# further evidence: the fingerprint must move again, or a turn that runs
# longer than the stall threshold reads as stalled while it is plainly
# working (the exact gap this probe exists to close for a headless runtime).
printf 'streamed output\n' >> "$log_file"
fp_grown="$("$activity" probe "$session" "$state_dir" | jq -r .fingerprint)"
if [ "$fp_grown" = "$fp_start" ]; then
  echo "FAIL mid-turn log growth changes the fingerprint: unchanged at '$fp_grown'" >&2
  fail=1
else
  echo "ok   mid-turn log growth changes the fingerprint"
fi

# The turn-boundary hooks still contribute independently of the log signal,
# and silence_expected still comes only from the hook half: a growing log
# is never grounds to call silence intended.
printf '{"hook_event_name":"UserPromptSubmit"}\n' | "$activity" working
working_envelope="$("$activity" probe "$session" "$state_dir")"
check "hook activity is not silence-expected" "false" "$(printf '%s' "$working_envelope" | jq -r .silence_expected)"
check "working reports its activity as the message" "state set-message selftest/session-1 working (codex UserPromptSubmit)" "$(tail -n 1 "$tmp/calls")"

# working: an unreachable plect never fails the hook, and state
# set-message's own failure is logged the same way event publish's is above.
PLECT_SESSION_NAME="$session" \
XDG_STATE_HOME="$XDG_STATE_HOME" \
PATH="$noplect_path" \
"$activity" working <<<'{"hook_event_name":"UserPromptSubmit"}'
[ $? -eq 0 ] || { echo "FAIL working must exit 0 even when plect is unreachable" >&2; fail=1; }
statelog="$XDG_STATE_HOME/plect/codex-activity/errors.log"
if [ -s "$statelog" ] && grep -q 'plect state set-message' "$statelog"; then
  echo "ok   an unreachable state set-message call is logged instead of the void"
else
  echo "FAIL an unreachable state set-message call should be logged to $statelog instead of the void" >&2
  fail=1
fi
: > "$statelog"

printf '{"hook_event_name":"Stop"}\n' | "$activity" waiting
waiting_envelope="$("$activity" probe "$session" "$state_dir")"
check "a completed turn's hook pardons silence" "true" "$(printf '%s' "$waiting_envelope" | jq -r .silence_expected)"
# The waiting phase marks a completed turn, not an activity: an idle session
# is reported as an empty message, not the literal word "waiting".
check "waiting clears the message instead of reporting itself as an activity" "state set-message selftest/session-1 " "$(tail -n 1 "$tmp/calls")"

fp_before_hooks="$fp_grown"
fp_after_hooks="$(printf '%s' "$waiting_envelope" | jq -r .fingerprint)"
if [ "$fp_after_hooks" = "$fp_before_hooks" ]; then
  echo "FAIL a turn-boundary hook still changes the fingerprint: unchanged at '$fp_after_hooks'" >&2
  fail=1
else
  echo "ok   a turn-boundary hook still changes the fingerprint"
fi

# A torn hook record (the write half's temp-file-then-rename closes the
# window this simulates directly, but the read half must still degrade
# gracefully rather than assume the write half is the only way to get here)
# must not fail the probe outright: with no log evidence either, that is
# indistinguishable from "no basis yet".
record_file="${XDG_STATE_HOME}/plect/codex-activity/$(printf '%s' "$session" | tr '/' '_').json"
mkdir -p "$(dirname "$record_file")"
printf '{"seq":3,"fingerprint":"working:3"' > "$record_file"
rm -rf "${state_dir:?}/log"
mkdir -p "$state_dir/log"
out="$("$activity" probe "$session" "$state_dir")"
check "a torn hook record with no log evidence degrades to empty output" "" "$out"

# The same torn record must not poison the log half: a turn's own log file
# is independent evidence, and the probe must still emit it.
touch "$state_dir/log/1800000000.jsonl"
torn_envelope="$("$activity" probe "$session" "$state_dir")"
check_nonempty "a torn hook record still emits valid log evidence when logs exist" "$torn_envelope"
check "the emitted fingerprint carries no hook half from the torn record" "hook=|log=1800000000.jsonl:0" \
  "$(printf '%s' "$torn_envelope" | jq -r .fingerprint)"

# reset drops all of it, hook record and log-derived state alike (the log
# directory itself is this test's fixture, not something reset owns, so it
# is only the hook half that reset can clear).
"$activity" reset "$session"
rm -rf "${state_dir:?}/log"
mkdir -p "$state_dir/log"
out="$("$activity" probe "$session" "$state_dir")"
check "reset drops the record" "" "$out"

run_report() {
  local payload="$1"
  : > "$tmp/calls"
  PLECT_SESSION_NAME="$session" \
  PLECT_CALLS="$tmp/calls" \
  XDG_STATE_HOME="$XDG_STATE_HOME" \
  PATH="$bin_dir:$PATH" \
  "$activity" reply <<<"$payload"
  cat "$tmp/calls"
}

# reply: a non-empty last_assistant_message publishes exactly one
# plect.message event, summary truncated to its first line, message_id
# deterministic (message_id_origin=synthetic) from turn_id, not a random one.
got="$(run_report '{"hook_event_name":"Stop","last_assistant_message":"line one\nline two","turn_id":"turn-abc"}')"
want='event publish selftest/session-1 --type plect.message --summary line one --body line one
line two --meta message_id=selftest/session-1/turn-abc --meta message_id_origin=synthetic --meta role=assistant --meta source=codex --meta turn_id=turn-abc'
check "reply publishes plect.message" "$want" "$got"
# 2 lines, not 1: the plect stub's own `printf '%s\n' "$*"` puts the whole
# call on one logical "line" of output but the call's own body embeds a
# newline ("line one\nline two"), splitting the file in two — a second
# actual call would add a third line with its own leading "event publish".
[ "$(wc -l < "$tmp/calls")" -eq 2 ] || { echo "FAIL reply published more than once: $got" >&2; fail=1; }

# reply: the same turn_id always mints the same message_id (deterministic,
# not a fresh random one on every call).
got2="$(run_report '{"hook_event_name":"Stop","last_assistant_message":"a different final answer","turn_id":"turn-abc"}')"
case "$got2" in
  *"message_id=selftest/session-1/turn-abc"*) echo "ok   reply message_id is deterministic per turn_id" ;;
  *) echo "FAIL reply message_id should be deterministic per turn_id, got: $got2" >&2; fail=1 ;;
esac

# reply: a trailing newline in last_assistant_message survives byte for byte
# -- command substitution silently strips trailing newlines unless guarded
# against, which would corrupt the canonical message text.
got="$(run_report '{"hook_event_name":"Stop","last_assistant_message":"paragraph one\n\n","turn_id":"turn-nl"}')"
want='event publish selftest/session-1 --type plect.message --summary paragraph one --body paragraph one

 --meta message_id=selftest/session-1/turn-nl --meta message_id_origin=synthetic --meta role=assistant --meta source=codex --meta turn_id=turn-nl'
check "reply preserves a trailing newline byte for byte" "$want" "$got"

# reply: an empty last_assistant_message publishes nothing.
got="$(run_report '{"hook_event_name":"Stop","last_assistant_message":""}')"
check "an empty reply publishes nothing" "" "$got"

# reply: a missing last_assistant_message field (not just empty) also
# publishes nothing.
got="$(run_report '{"hook_event_name":"Stop"}')"
check "a reply with no field publishes nothing" "" "$got"

# reply: no turn_id falls back to a deterministic per-session counter (never
# a random id), still distinct call to call.
got="$(run_report '{"hook_event_name":"Stop","last_assistant_message":"hi"}')"
case "$got" in
  *"message_id=selftest/session-1/reply-0"*) echo "ok   reply with no turn_id uses the reply-seq fallback" ;;
  *) echo "FAIL reply with no turn_id should use the reply-seq fallback, got: $got" >&2; fail=1 ;;
esac
case "$got" in
  *turn_id*) echo "FAIL reply with no turn_id should carry no turn_id, got: $got" >&2; fail=1 ;;
  *) echo "ok   reply with no turn_id carries no turn_id metadata" ;;
esac
got="$(run_report '{"hook_event_name":"Stop","last_assistant_message":"hi again"}')"
case "$got" in
  *"message_id=selftest/session-1/reply-1"*) echo "ok   reply fallback counter advances on the next call" ;;
  *) echo "FAIL reply fallback counter should advance on the next call, got: $got" >&2; fail=1 ;;
esac

# reply: plect being unreachable never fails the turn, and its failure is
# logged instead of silently discarded.
: > "$tmp/calls"
PLECT_SESSION_NAME="$session" \
XDG_STATE_HOME="$XDG_STATE_HOME" \
PATH="$noplect_path" \
"$activity" reply <<<'{"hook_event_name":"Stop","last_assistant_message":"secret-payload-marker"}'
[ $? -eq 0 ] || { echo "FAIL reply must exit 0 even when plect is unreachable" >&2; fail=1; }
errlog="$XDG_STATE_HOME/plect/codex-activity/errors.log"
if [ -s "$errlog" ] && grep -q 'plect event publish' "$errlog"; then
  echo "ok   an unreachable plect call is logged instead of the void"
else
  echo "FAIL an unreachable plect call should be logged to $errlog instead of the void" >&2
  fail=1
fi
if grep -q 'secret-payload-marker' "$errlog"; then
  echo "FAIL errors.log leaked the assistant message payload" >&2
  fail=1
else
  echo "ok   errors.log does not leak the assistant message payload"
fi
: > "$errlog"

# reset must NOT drop the reply-seq counter: exec_runtime's setup calls
# reset on every launch, resumes included, and its reply calls never carry
# a turn_id to fall back from -- restarting the counter would mint an id a
# prior turn already published, reusing reply-0 across a restart instead of
# advancing past it.
[ -s "$XDG_STATE_HOME/plect/codex-activity/selftest_session-1.reply-seq" ] || { echo "FAIL expected a reply-seq counter file before reset" >&2; fail=1; }
"$activity" reset "$session"
[ -s "$XDG_STATE_HOME/plect/codex-activity/selftest_session-1.reply-seq" ] || { echo "FAIL reset must not remove the session's reply-seq counter" >&2; fail=1; }
got="$(run_report '{"hook_event_name":"Stop","last_assistant_message":"after a reset"}')"
# reply-3, not reply-2: the unreachable-plect call just above still advanced
# the counter (that logic runs before the publish attempt), consuming
# reply-2 even though nothing was actually published for it.
case "$got" in
  *"message_id=selftest/session-1/reply-3"*) echo "ok   the reply-seq counter survives reset and keeps advancing" ;;
  *) echo "FAIL a reset should not reset the reply-seq counter, got: $got" >&2; fail=1 ;;
esac

[ "$fail" -eq 0 ] || { echo "codex-agent-activity selftest failed" >&2; exit 1; }
echo "codex-agent-activity selftest passed"

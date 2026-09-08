#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
subject="$script_dir/claude-agent-activity"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

bin_dir="$tmp/bin"
mkdir -p "$bin_dir"
cat > "$bin_dir/plect" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PLECT_CALLS"
EOF
chmod +x "$bin_dir/plect"

run_hook() {
  local payload="$1"
  PLECT_SESSION_NAME="owner/repo-1" \
  PLECT_CALLS="$tmp/calls" \
  XDG_STATE_HOME="$tmp/state" \
  PATH="$bin_dir:$PATH" \
  "$subject" working <<<"$payload"
  tail -n 1 "$tmp/calls"
}

got="$(run_hook '{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"go test ./app/... --token secret"}}')"
want='state set-message owner/repo-1 working: Bash go'
[ "$got" = "$want" ] || { printf 'Bash text = %q, want %q\n' "$got" "$want" >&2; exit 1; }

got="$(run_hook '{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"/tmp/private/runtime.toml"}}')"
want='state set-message owner/repo-1 working: Edit runtime.toml'
[ "$got" = "$want" ] || { printf 'Edit text = %q, want %q\n' "$got" "$want" >&2; exit 1; }

got="$(run_hook '{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/tmp/private/with space.txt"}}')"
want='state set-message owner/repo-1 working: Read with space.txt'
[ "$got" = "$want" ] || { printf 'Read text = %q, want %q\n' "$got" "$want" >&2; exit 1; }

got="$(run_hook '{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"/usr/local/bin/deploy --password hunter2"}}')"
case "$got" in
  *password*|*hunter2*) printf 'Bash text leaked command details: %s\n' "$got" >&2; exit 1 ;;
esac
want='state set-message owner/repo-1 working: Bash /usr/local/bin/deploy'
[ "$got" = "$want" ] || { printf 'Bash path command text = %q, want %q\n' "$got" "$want" >&2; exit 1; }

long_name="very-long-generated-file-name-that-would-overflow-the-slack-status-line-runtime.toml"
got="$(run_hook "{\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"Edit\",\"tool_input\":{\"file_path\":\"/tmp/private/$long_name\"}}")"
text="${got#state set-message owner/repo-1 }"
[ "${#text}" -le 80 ] || { printf 'status text length = %d, want <=80: %s\n' "${#text}" "$text" >&2; exit 1; }

got="$(run_hook '{"hook_event_name":"UserPromptSubmit"}')"
want='state set-message owner/repo-1 working'
[ "$got" = "$want" ] || { printf 'UserPromptSubmit text = %q, want %q\n' "$got" "$want" >&2; exit 1; }

# The Stop hook's waiting phase clears the message rather than reporting the
# literal word "waiting" as if it were a current activity.
PLECT_SESSION_NAME="owner/repo-1" \
PLECT_CALLS="$tmp/calls" \
XDG_STATE_HOME="$tmp/state" \
PATH="$bin_dir:$PATH" \
"$subject" waiting <<<'{"hook_event_name":"Stop"}'
got="$(tail -n 1 "$tmp/calls")"
want='state set-message owner/repo-1 '
[ "$got" = "$want" ] || { printf 'Stop text = %q, want %q\n' "$got" "$want" >&2; exit 1; }

run_report() {
  local verb="$1" payload="$2"
  : > "$tmp/calls"
  PLECT_SESSION_NAME="owner/repo-1" \
  PLECT_CALLS="$tmp/calls" \
  XDG_STATE_HOME="$tmp/state" \
  PATH="$bin_dir:$PATH" \
  "$subject" "$verb" <<<"$payload"
  cat "$tmp/calls"
}

# reply: a non-empty last_assistant_message publishes exactly one
# plect.message event, summary truncated to its first line, message_id
# minted (message_id_origin=synthetic) since Stop carries none of its own.
got="$(run_report reply '{"hook_event_name":"Stop","last_assistant_message":"line one\nline two","prompt_id":"turn-abc"}')"
case "$got" in
  'event publish owner/repo-1 --type plect.message --summary line one --body line one'$'\n''line two --meta message_id='*' --meta message_id_origin=synthetic --meta role=assistant --meta source=claude --meta turn_id=turn-abc') ;;
  *) printf 'reply text = %q\n' "$got" >&2; exit 1 ;;
esac
[ "$(wc -l < "$tmp/calls")" -eq 2 ] || { printf 'reply published more than once: %s\n' "$got" >&2; exit 1; }

# reply: an empty last_assistant_message publishes nothing.
got="$(run_report reply '{"hook_event_name":"Stop","last_assistant_message":""}')"
[ -z "$got" ] || { printf 'empty reply should publish nothing, got: %s\n' "$got" >&2; exit 1; }

# reply: a missing last_assistant_message field (not just empty) also
# publishes nothing.
got="$(run_report reply '{"hook_event_name":"Stop"}')"
[ -z "$got" ] || { printf 'reply with no field should publish nothing, got: %s\n' "$got" >&2; exit 1; }

# reply: no prompt_id means no turn_id metadata, rather than an empty one.
got="$(run_report reply '{"hook_event_name":"Stop","last_assistant_message":"hi"}')"
case "$got" in
  *turn_id*) printf 'reply with no prompt_id should carry no turn_id, got: %s\n' "$got" >&2; exit 1 ;;
esac

# reply: plect being unreachable never fails the turn.
: > "$tmp/calls"
PLECT_SESSION_NAME="owner/repo-1" \
XDG_STATE_HOME="$tmp/state" \
PATH="$(dirname "$(command -v jq)")" \
"$subject" reply <<<'{"hook_event_name":"Stop","last_assistant_message":"hi"}'
[ $? -eq 0 ] || { echo "reply must exit 0 even when plect is unreachable" >&2; exit 1; }

# message_display: a non-final delta publishes exactly one
# plect.message_delta event whose body is that delta alone, not any running
# concatenation, carrying message_id/index/final/turn_id as metadata — and
# no plect.message yet, since the message isn't finished.
got="$(run_report message_display '{"hook_event_name":"MessageDisplay","message_id":"msg-1","turn_id":"turn-abc","index":0,"final":false,"delta":"Hello "}')"
want='event publish owner/repo-1 --type plect.message_delta --summary Hello  --body Hello  --meta message_id=msg-1 --meta message_id_origin=native --meta kind=text --meta index=0 --meta final=false --meta source=claude --meta turn_id=turn-abc'
[ "$got" = "$want" ] || { printf 'message_display (non-final) = %q, want %q\n' "$got" "$want" >&2; exit 1; }

# message_display: the final delta for the same message_id publishes its own
# plect.message_delta, then a plect.message whose body is every delta seen
# for that message_id concatenated, not just the final one.
got="$(run_report message_display '{"hook_event_name":"MessageDisplay","message_id":"msg-1","turn_id":"turn-abc","index":1,"final":true,"delta":"world"}')"
want='event publish owner/repo-1 --type plect.message_delta --summary world --body world --meta message_id=msg-1 --meta message_id_origin=native --meta kind=text --meta index=1 --meta final=true --meta source=claude --meta turn_id=turn-abc
event publish owner/repo-1 --type plect.message --summary Hello world --body Hello world --meta message_id=msg-1 --meta message_id_origin=native --meta role=assistant --meta source=claude --meta turn_id=turn-abc'
[ "$got" = "$want" ] || { printf 'message_display (final) = %q, want %q\n' "$got" "$want" >&2; exit 1; }

# message_display: that final delta leaves a marker so a same-turn Stop does
# not publish a second plect.message for text MessageDisplay already covered
# — and Stop consumes (clears) the marker so the next turn is unaffected.
got="$(run_report reply '{"hook_event_name":"Stop","last_assistant_message":"Hello world","prompt_id":"turn-abc"}')"
[ -z "$got" ] || { printf 'Stop after a MessageDisplay final should publish nothing, got: %s\n' "$got" >&2; exit 1; }
got="$(run_report reply '{"hook_event_name":"Stop","last_assistant_message":"next turn","prompt_id":"turn-def"}')"
case "$got" in
  event\ publish*) ;;
  *) printf 'Stop on the next turn should publish again once the marker is consumed, got: %s\n' "$got" >&2; exit 1 ;;
esac

# message_display: no turn_id means no turn_id metadata, rather than an
# empty one — on both the delta and the message it completes.
got="$(run_report message_display '{"hook_event_name":"MessageDisplay","message_id":"msg-2","index":0,"final":true,"delta":"hi"}')"
case "$got" in
  *turn_id*) printf 'message_display with no turn_id should carry no turn_id, got: %s\n' "$got" >&2; exit 1 ;;
esac

# message_display: an empty delta publishes no plect.message_delta (empty
# text), and a final flush whose buffered text is still empty publishes no
# plect.message either.
got="$(run_report message_display '{"hook_event_name":"MessageDisplay","message_id":"msg-3","index":0,"final":true,"delta":""}')"
[ -z "$got" ] || { printf 'empty message_display should publish nothing, got: %s\n' "$got" >&2; exit 1; }

# message_display: no message_id means the payload didn't parse as this
# hook's expected shape, so nothing publishes.
got="$(run_report message_display '{"hook_event_name":"MessageDisplay","index":0,"final":true,"delta":"hi"}')"
[ -z "$got" ] || { printf 'message_display with no message_id should publish nothing, got: %s\n' "$got" >&2; exit 1; }

# message_display: plect being unreachable never fails the turn.
PLECT_SESSION_NAME="owner/repo-1" \
XDG_STATE_HOME="$tmp/state" \
PATH="$(dirname "$(command -v jq)")" \
"$subject" message_display <<<'{"hook_event_name":"MessageDisplay","message_id":"msg-4","index":0,"final":true,"delta":"hi"}'
[ $? -eq 0 ] || { echo "message_display must exit 0 even when plect is unreachable" >&2; exit 1; }

# reset also drops the message-buffer directory and the last-emitted marker,
# so a crashed turn cannot leak partial text or a stale suppression into the
# next run.
run_report message_display '{"hook_event_name":"MessageDisplay","message_id":"msg-5","index":0,"final":false,"delta":"orphaned"}' >/dev/null
[ -e "$tmp/state/plect/claude-activity/owner_repo-1.messages/msg-5" ] || { echo "expected an orphaned buffer file before reset" >&2; exit 1; }
run_report message_display '{"hook_event_name":"MessageDisplay","message_id":"msg-6","index":0,"final":true,"delta":"done"}' >/dev/null
[ -s "$tmp/state/plect/claude-activity/owner_repo-1.last-message-id" ] || { echo "expected a last-message-id marker before reset" >&2; exit 1; }
XDG_STATE_HOME="$tmp/state" "$subject" reset "owner/repo-1"
[ ! -e "$tmp/state/plect/claude-activity/owner_repo-1.messages" ] || { echo "reset must remove the session's message buffer directory" >&2; exit 1; }
[ ! -e "$tmp/state/plect/claude-activity/owner_repo-1.last-message-id" ] || { echo "reset must remove the session's last-message-id marker" >&2; exit 1; }

echo "claude-agent-activity selftest passed"

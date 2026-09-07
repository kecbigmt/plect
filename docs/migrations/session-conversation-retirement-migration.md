# Session conversation retirement migration

`Session.Conversation` (`contracts/state.Conversation`: `source`, `url`,
`metadata`), the `plect state set-conversation` command, and the
`conversation` line in the Web UI's session detail page are removed. The
facts that field held — a Slack thread's `permalink`, `thread_ts`, and
`channel_id` — already live under the node that produced them, in that
node's own outputs. `plect status --json --full` (and `plect status`'s Work
section generally) exposes those outputs directly; nothing else in Plecture
read `Conversation` except the CLI command and the Web UI line this change
also removes.

The change is intentionally breaking. Plecture is pre-1.0, so authors
migrate their own configuration and read sites once instead of relying on a
compatibility shim.

## Who is affected

- A workflow or task definition (a global `~/.config/plect/` layer, a repo
  overlay, or a plugin) that calls `plect state set-conversation` from a
  setup or cleanup script.
- A script or tool that reads `session.conversation` from `plect status
  --json` output, or `.conversation` from a SQLite `sessions.record_json`
  blob inspected directly.

A session's own runtime behavior is unaffected: the shipped `slack_thread`
effect and `thread_workspace` workspace provider still report `thread_ts`,
`channel_id`, and `permalink` as outputs exactly as before — only the
redundant copy into `Session.Conversation` is gone.

## Find every call site

```bash
grep -rn "state set-conversation" ~/.config/plect .plect /path/to/your-plugin/config
```

Drop the roots that do not apply to you. A plugin you merely consume needs
no edit from you — its author migrates it, and a plugin declaration still
invoking the retired command fails at run time with a "unknown command"
error naming it.

## Remove a `state set-conversation` call

Before:

```sh
plect state set-conversation "$session_name" \
  --source Slack \
  --url "$permalink" \
  --meta "thread_ts=$thread_ts" \
  --meta "channel_id=$channel_id"
```

After: delete the call. If the surrounding script produced `$permalink`,
`$thread_ts`, and `$channel_id` only to feed this call, and the setup's
`outputs_schema` does not already declare them, add them there so the same
facts remain readable — this is what the shipped `slack_thread` effect does.

## Update a read site

Before, `plect status --json` carried:

```json
{
  "runtime": {
    "conversation": {
      "source": "Slack",
      "url": "https://example.slack.com/archives/C01/p123",
      "metadata": { "thread_ts": "123.456", "channel_id": "C01" }
    }
  }
}
```

After, the same facts are readable under the node that produced them, in
`work[].outputs`:

```json
{
  "work": [
    {
      "instance": "slack_thread",
      "outputs": {
        "permalink": "https://example.slack.com/archives/C01/p123",
        "thread_ts": "123.456",
        "channel_id": "C01"
      }
    }
  ]
}
```

The node id, and which of its outputs carry the facts you need, depend on
which effect or workspace provider produced them — the shipped Slack plugin
uses `permalink`, `thread_ts`, and `channel_id` on both `slack_thread` and
`thread_workspace`, but a third-party plugin's effect may name its outputs
differently.

## Existing durable state

A pre-existing `state.json` (or a SQLite `sessions.record_json` blob) may
still carry a `"conversation"` key from before this change. It is not
read or migrated — plect from this point on ignores it, the same as any
other unrecognized key in that JSON blob. Removing it is optional cleanup,
not required for correctness.

## Verify

Re-run the grep from [Find every call site](#find-every-call-site). No hits
across your layers is the completion condition.

```bash
plect status <session> --json --full
```

confirms the facts you need are visible under the producing node's outputs,
and that no `conversation` key appears under `runtime`.

# Judge verdict fields are `judge_*`, not `reviewer_*`

The judge → judged relationship was previously named after one of its use
cases (`reviewer_session` / `reviewer_workflow`). It is now named after the
relationship itself: `judge_session` / `judge_workflow`.

This renames:

- `contracts/state.DoneWhenJudge`'s `ReviewerSession`/`ReviewerWorkflow`
  fields (JSON tags `reviewer_session`/`reviewer_workflow`) to
  `JudgeSession`/`JudgeWorkflow` (`judge_session`/`judge_workflow`). The
  same record drops its `TargetSession`/`Instance` fields — the judged
  session/instance was always the task instance the record is stored
  under, so nothing reads them from the record itself any more.
- The `plect_judge_approve` / `plect_judge_request_changes` MCP tools'
  `reviewer_session` argument to `judge_session`.
- `plect judge approve` / `plect judge request-changes`'s
  `--reviewer-session` flag to `--judge-session`.
- The `plect.judge.recorded` event's `reviewer_session`/`reviewer_workflow`
  metadata keys to `judge_session`/`judge_workflow`.

The change is intentionally breaking. Plecture is pre-1.0, so a caller that
names the old argument fails with an unknown-flag/unknown-argument error
rather than being silently accepted under the old name.

## Backup

Before editing anything, copy the configuration layers you are about to
grep against a timestamped backup:

```bash
CONFIG_HOME="${PLECT_CONFIG_HOME:-$HOME/.config/plect}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
cp -r "$CONFIG_HOME" "$CONFIG_HOME.migration-backup.$STAMP"
```

Repeat for any repo overlay (`.plect/`) or plugin source tree you maintain.

## Find every reference

```bash
grep -rn "reviewer_session\|reviewer-session\|reviewer_workflow\|ReviewerSession\|ReviewerWorkflow" \
  "$CONFIG_HOME" .plect docs 2>/dev/null
```

This repository's own shipped catalog (`plugins/github`, `plugins/okf`) and
task documents invoke `plect judge approve`/`request-changes` without ever
passing `--reviewer-session` explicitly — the flag always defaults to
`$PLECT_SESSION_NAME`, so no shipped task document needed a text change for
this rename. A downstream overlay or a wrapper script that *does* pass the
flag or the MCP argument by name needs the rewrite below; one that never
named it needs no edit.

## Rewrite a caller that names the flag or argument explicitly

CLI:

```bash
plect judge approve <session> <instance> <judge-id> --reason "<reason>" \
  --judge-session "<name>"          # was --reviewer-session
```

MCP tool call (`plect_judge_approve` / `plect_judge_request_changes`):

```json
{"session": "...", "instance": "...", "judge_id": "...", "reason": "...",
 "judge_session": "..."}            // was "reviewer_session"
```

A channel binding or dashboard that reads the `plect.judge.recorded` event's
metadata by key takes the same rewrite:

```
judge_session   // was reviewer_session
judge_workflow  // was reviewer_workflow
```

## Verify

Re-run the grep from [Find every reference](#find-every-reference). No hits
across your layers is the completion condition.

## Nothing else moves

- **No `state.json`/SQLite content migration.** This is a code- and
  argument-level rename; it changes how a *new* verdict is named when
  recorded and read back. It does not touch the not-yet-built
  `plect storage import` state.json → SQLite cutover tool (see
  `docs/design/sqlite-persistence.md`), which is a separate migration with
  its own procedure; that importer maps whatever key name a legacy
  `state.json` holds at the time it runs to the current contract's
  `judge_session`/`judge_workflow`.
- **The relation policy, self-review rejection, and stale-revision
  handling are unchanged.** Only the name of the judge identity fields
  moved; the semantics of who a judge leaf accepts did not change.

# Health declaration

This design is governed by
[`../adr/2026-08-18-health-declaration.md`](../adr/2026-08-18-health-declaration.md)
and, for the activity envelope's shape,
[`../adr/2026-08-18-activity-envelope-fingerprint-centric.md`](../adr/2026-08-18-activity-envelope-fingerprint-centric.md).

## Design Core

An effect declares how its health is determined in one `[health]` table
holding two probes:

- **`alive`** — the liveness probe. Exit-code semantics: zero means the
  execution surface this effect owns is present.
- **`activity`** — the activity probe. Fingerprint semantics: it writes a JSON
  activity envelope whose opaque fingerprint core compares across evaluations,
  and its exit code reports whether the probe itself is well.

`setup`, `cleanup`, and `[health]` are the universal effect lifecycle trio:
what brings the surface up, what takes it down, and what says whether it is
still there and moving.

Probes are plugin-shipped capability. A plugin that owns a surface ships the
probes for it, so default health coverage requires no user configuration.

## Worked example

```toml
# The tmux plugin's pane effect: liveness is a session lookup, and activity is
# a fingerprint of the pane's visible contents.
[pane.health.alive]
type   = "shell"
script = 'tmux has-session -t "$session_name"'

[pane.health.alive.bind]
session_name = { from = "self.outputs.session_name" }

[pane.health.activity]
type   = "shell"
script = '''
PANE=$(tmux capture-pane -p -t "$session_name") || exit 1
FP=$(printf '%s' "$PANE" | cksum | tr -d ' \t')
jq -nc --arg fp "$FP" --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{fingerprint: $fp, observed_at: $at}'
'''

[pane.health.activity.bind]
session_name = { from = "self.outputs.session_name" }
```

```toml
# An agent-runtime effect: liveness is a process check, and activity is the
# turn-boundary record the same plugin's hook writes.
[exec_runtime.health.alive]
type   = "shell"
script = 'kill -0 "$pid"'

[exec_runtime.health.alive.bind]
pid = { from = "self.outputs.pid" }

[exec_runtime.health.activity]
type = "exec"
bin  = "codex-agent-activity"
args = ["probe", { from = "session.name" }, { from = "self.outputs.state_dir" }]
```

A hook and a pane fingerprint are two implementations of one probe. Both reach
core through `activity`, so core has one probe shape to evaluate rather than
one per implementation technique.

## Validation rules

- `[health]` is optional for an effect declaring no `setup`; such an effect
  contributes nothing to health. An effect that declares `setup` declares
  `[health.alive]`, so that "not observed" is a reviewed choice rather than an
  omission.
- `activity` is optional and independent of `alive`: neither implies the
  other. A `[health]` table declaring neither is a load error — the only
  reason to write the header is to declare a probe.
- Unknown keys under `[health]` are a load error naming the offending key. A
  readiness-style third probe is a non-goal: health answers "is this surface
  present and moving", not "may traffic be sent".
- Each member is an action: exec, shell, or, for `alive` only, `noop` — a
  declared non-observation, with its values declared in `bind` or in `args`
  rather than interpolated into the script. `noop` is legal only under
  `[health.alive]`; the rule applies independently to every layer of a nesting
  chain, and a `noop` layer does not suppress an inner layer's own executable
  probe.
- Both members observe the same roots `cleanup` does, minus the ones a probe
  has no business reading: the effect's own outputs (`self.outputs.<key>`),
  its resolved inputs (`inputs.<key>`), and the session and workspace. A
  `bin = "<name>"` reference resolves against the declaring plugin.
- An effect declaration is replaced wholesale by a deeper cascade layer, so a
  user-owned overlay of a plugin effect carries its own complete `[health]`.

## Probe semantics

Either probe is an action, so what runs follows the variant it declares: an
`exec` probe runs its declared executable with its resolved argv, and a
`shell` probe writes its literal script and a binding file into a private run
directory and invokes the generated wrapper. Neither substitutes anything into
the script.

A value the probe reads may fail to resolve, which happens before any process
starts. That failure reaches the health report the same way a failing
execution does — `alive` makes the session unhealthy, `activity` records a
probe error — because a probe that cannot be resolved is a probe that cannot
answer, and a declared probe answering nothing is the case health exists to
report.

### alive

Exit zero means the surface is present. A non-zero exit, or a value that does
not resolve, makes the session unhealthy, and the failing instance is named in
the reported reason. The first failing probe ends the evaluation.

### activity

Stdout decides what the instance contributes, and the exit code decides the
health of the probe itself. The two are orthogonal: a probe
that contributes nothing may be perfectly well, and a broken probe is a fault
whatever it managed to print.

Stdout is the activity envelope, which has exactly two legal shapes:

```json
{ "fingerprint": "waiting:7", "observed_at": "2026-08-18T00:04:06Z" }
{ "fingerprint": "waiting:7", "silence_expected": true, "observed_at": "2026-08-18T00:04:06Z" }
```

| Field | Meaning |
|---|---|
| `fingerprint` | An opaque token that changes whenever the probe observes new activity. Core never parses it, only compares it. Required. |
| `silence_expected` | `true` says this fingerprint's stability is intended, so its silence is pardoned: the declaring instance's expectation is narrowed. Absent or `false` withholds the pardon. |
| `observed_at` | RFC3339. Absent means the probe reports no timestamp of its own. |

The envelope is fingerprint-centric: it always carries evidence, and
`silence_expected` annotates how that evidence's stability should be read. A
pardon therefore cannot exist without evidence to pardon.

A probe holds only the pardon channel — it may lower the expectation core
derived from `done_when`, never raise one. The asymmetry is deliberate: a
wrong pardon hides a real stall, while a wrongly withheld pardon is safe,
because core's own `done_when`-derived expectation still has to agree before
anything is called stalled. The accusation is always core's. So a generic
surface fingerprint withholds `silence_expected` — a pane's contents cannot
establish that quiet is intended — while a turn-boundary probe sets it once
the turn it watches has ended.

Four outcomes contribute no activity evidence for an evaluation, and they
differ only in what the health report says about the probe:

| Outcome | Shape | Health report |
|---|---|---|
| No basis | Exit 0, empty stdout. | Silent — the instance evaluates as if it declared no probe at all. |
| Probe error | Non-zero exit. | A `probe_error` entry naming the instance, the probe command, the exit code, and a digest of stderr. |
| Invalid output | Exit 0, stdout that is not an envelope carrying a `fingerprint`. | A `probe_error` entry naming the instance, the command, and the reason. |
| Unresolved value | No process ran: a value the probe reads resolved to nothing and declared neither a default nor `optional`. | A `probe_error` entry naming the instance, the command, and the value that did not resolve. |

"No basis" is structural absence rather than a declared value, so a probe with
nothing to report cannot be confused with one whose output was rejected. A
persistently broken probe surfaces as a fault instead of passing for a quiet
surface, which is why a fingerprint-less envelope is rejected rather than
discarded or filled in.

Freshness is judged against core's own clock, not against `observed_at`: core
records when it first saw a given fingerprint, and a fingerprint that has not
changed within the workflow's stall threshold reads as stalled once activity
is expected.

## Composition across instances

A session's health is composed from every produced run-scoped effect instance:

- **`alive` composes by AND.** Liveness is a chain of necessary resources, so
  any failing probe makes the session unhealthy, reported with the failing
  instance named.
- **`activity` composes by OR.** Activity is evidence of life, so evidence
  from any instance counts. Core joins the declaring instances' fingerprints
  into one composite and treats a change anywhere in it as the session having
  moved.

An instance declaring no `[health]` contributes nothing to either composition:
it is vacuous in the AND and casts no vote in the OR. So does one whose
activity probe emits no envelope.

The two shapes are complementary in practice. A pane fingerprint attests
within-turn movement that no turn boundary would show, and a turn-boundary
record attests movement a quiet pane would hide.

Declare an activity probe only for resources whose activity indicates session
progress: a chatty sidecar with an activity probe would mask a stalled agent,
so the author-declared-surface principle applies to probe placement too.

## Relationship to the workflow health cycle

A workflow's `[healthcheck]` table declares the sampling cycle — how often the
probes run, the stall threshold they are judged against, and the parent
re-notification interval. It names when health is observed; `[health]` names
what health means for one effect.

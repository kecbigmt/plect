# Graph inspection field mapping

[docs/design/web-ui.md](web-ui.md)'s Graph section specifies seven detail
rows a node's shared detail pane shows. This document maps each row to the
existing state/service source that carries it, ahead of the Graph task
itself (`docs/design/web-ui.md`'s Delivery sequence). It does not define an
API, endpoint, or DTO — see
[docs/adr/2026-09-06-web-api-schema-contract.md](../adr/2026-09-06-web-api-schema-contract.md)
for where that lives when the Graph task builds it.

| Detail | Source | Availability |
| --- | --- | --- |
| Structure | `task.Plan` / `task.Resolved` (`app/internal/task/task.go`): `NodeID`, `TaskID`, `Scope`, `DependsOn`, `Inputs map[string]*lang.Value` (binding expressions), `Layers []effect.Layer` for a nested node. | Available. Resolved against the session's *current* config, not a frozen definition snapshot — see the caveat below. |
| Result | `contract.TaskState`: `Status`, `SetupAt`, `FailedAt`, `CleanedAt`, `Error` (`contracts/state/state.go`). | Available for the instance's current/latest terminal state. Not available: a history of every setup/failure/cleanup *attempt* — the contract keeps one set of timestamps per instance, not an attempt log. |
| Inputs | Definition binding: `task.Resolved.Inputs` (`map[string]*lang.Value`, unresolved expressions). Persisted resolved value: `contract.TaskState.Inputs` (`map[string]any`, post-template, set at setup). | Available, and already two distinct things per the design doc's own wording — the binding expression and the value it last resolved to are separate reads, not one field with two names. |
| Outputs | `contract.TaskState.Outputs` (`map[string]any`, parsed JSON from setup stdout); `OutputsSchema`/`MutableOutputs` on `task.Resolved` distinguish which keys `plect state set-output` may later change. | Available. |
| Layers | `contract.TaskState.Layers []contract.LayerState`: per layer `EffectID`, `Status`, `Inputs`, `Locals`, `Outputs`, `Env`, `SetupAt`/`FailedAt`/`CleanedAt`, `Error`. Structural (pre-execution) side: `effect.Layer` on `task.Resolved.Layers`. | Available. |
| Stored metadata | `contract.TaskState.Scope`, `.Seq`, `.Resource`, `.Name`; whether an instance is a workflow-DAG node or a dynamic `plect task setup` instance is which of `Session.Nodes` / `Session.Tasks` its key is found in, not a field on the record itself. | Available. |
| Events | `event.Event` (`contracts/event/event.go`) carries no structured node/instance correlation field — no `NodeID`/`TaskID`/`Instance` column exists on the event contract, only a free-form `Metadata map[string]string` and a dotted `Type` string that different event kinds populate by their own convention, not a general one. | Partially available: an event kind that happens to record the instance in `Metadata` or encode it in `Type` is identifiable by that convention; nothing guarantees every node-related event does. A link to the session timeline is always available and is the documented fallback. |

## Caveats, not gaps

- **Structure reflects current config, not a frozen snapshot.** A session's
  `Workflow` name resolves against whatever that workflow's definition is
  *now*; if it changed since the session ran, the displayed structure and
  bindings may not match what actually executed. This is a correctness
  caveat on data that does exist, not missing data — `docs/design/web-ui.md`
  already states it as a hard rule ("Do not label a binding from available
  configuration as the expression used in a past execution").
- **Channels are out of the node graph by design**, not because their
  wiring is unavailable — `docs/design/web-ui.md` keeps them a distinct
  concern from the Effect graph. Mapping channel delivery-time wiring to a
  display is unstarted work, tracked by the Graph task itself rather than
  by this document.

## Unavailable fields for later implementation

- **Per-attempt result history.** `contract.TaskState`/`LayerState` keep one
  terminal timestamp set per instance/layer; a node retried after failure
  has no record of the earlier attempt(s) to show.
- **General event-to-node correlation.** Extending `event.Event` with a
  structured instance/node reference (rather than relying on
  per-event-kind `Metadata`/`Type` convention) is unimplemented; the Graph
  task decides whether it is worth adding versus keeping the session
  timeline link as the answer for most event kinds.

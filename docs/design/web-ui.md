# Plecture Web UI

The Plecture Web UI preserves the hierarchy and context of work while making
recorded session activity inspectable. Sessions are the primary workspace;
execution structure is available when investigation calls for it.

The [architecture decision](../adr/2026-09-05-web-ui-client-server-boundary.md)
defines the implementation boundary. The runnable
[reference prototype](web-ui/prototype/README.md) illustrates layout and
interaction, not an API or state contract.

## Scope and source of truth

The UI serves individual use. Labels are English; user-authored work content
can be in any language. Instructional copy is minimal. Training, workflow
definition browsing/editing, team administration, and a separate analytics
dashboard are outside the initial scope.

The UI uses existing service operations and persisted records. HTTP adapters
and display projections are implementation work; they do not introduce new
language constructs, state fields, event semantics, or lifecycle operations.
The browser owns drafts, selection, expansion, and scroll position. It does
not maintain a separate business-state model.

The following dimensions remain distinct:

| Subject | Values and interpretation |
| --- | --- |
| Session run | `up / down` |
| Session health | `healthy / unhealthy / undeclared / stalled` |
| Recorded node lifecycle | `produced / failed / cleaned`; produced does not mean business success or continued execution |
| Task completion | `satisfied / unsatisfied / pending`; false and not yet knowable are different |
| Action | Existing service decisions, including `review_required`; not necessarily a human approval request |

Message and workflow display status are separate information. The UI does
not infer a session-wide Working, Needs input, or Done state.

## Navigation and workspace

The left sidebar contains Home, Inbox, and a session tree. The center contains
one session workspace with flat Conversation, Tasks, Graph, and Terminal tabs.
A single optional right pane displays Session, Task, or Node details.

### Session tree and header

- Build the tree from `ParentSession`, not slashes in names or resource IDs.
  Use stable sibling ordering; new activity does not move rows.
- Separate expansion from selection. Parents remain selectable. Opening a
  session through another view expands its ancestors.
- Preserve expansion and scroll position. Search session names and resources,
  retaining ancestors of matching sessions.
- Keep independent roots distinct. Explicit sibling relationships and implicit
  `root:<name>` parents do not justify inventing selectable parent sessions.
  Records with missing parents remain visible.
- Display session `name` consistently in the tree, header, and details. The
  header has no breadcrumbs and little vertical space between name and resource.
  It also exposes Details and existing Up/Down operations.
- Workflow `display` can project a title, but the primary label uses name.
  Do not add a separately persisted free-text session title.
- Creation follows the existing Create contract and workflow input schema.
  A mock Name field, fixed agent, or Initial task selector is not an API
  requirement. A task input appears only when the selected workflow declares it.

Manual reparenting, arbitrary folders, drag ordering, and descendant unresolved
request counts are outside the initial scope.

### Conversation

[Event history and the history/live handoff protocol](web-ui-event-history.md)
specifies the bounded read contract behind this tab and how a client moves
from that read to a live subscription without losing or duplicating an
event.

Render readable utterances around their bodies and other events as compact
timeline entries. Show recorded type, source, summary, body, and metadata for
unknown event kinds. Display task, node, and child-session links only when
recorded references support them. Do not synthesize history from state changes.

The timeline represents Plecture's event log, not guaranteed transcripts from
every agent or external chat. The persisted Conversation field describes an
external conversation's source, URL, and metadata; it is not a transcript.
Expose its external URL when available.

The session timeline contains that session's records, including notifications
delivered through existing routes. It does not silently include every descendant
event. For notifications, distinguish the receiving `session_name` from
`origin_session`.

The composer publishes `user.emit` through the existing service operation.
An appended event is not proof of agent delivery or completion; delivery depends
on workflow channel wiring. No synthetic agent reply follows a successful write.

### Tasks

List actual Task instances belonging to the selected session. Identity is the
session plus instance key, not the resource string. Internal storage names such
as `Session.Tasks` do not make every stored execution element a language Task.

Task details show resource, lifecycle, done_when, condition evidence,
`State` (`self.state`), `Observed` (`resource.state` and observation time),
Action, and relevant Chain information. Keep judge decisions
`approve / request_changes` distinct from task-specific verdict strings.

Instruction text comes from recorded instruction events or another existing
record. Re-rendering an edited definition does not reconstruct a past instruction.
No new task-management or judgment operation is implied by this view.

An initial resource observation failure can prevent instance creation; do not
invent a pending instance. A subsequent observation failure can leave an
existing instance with unavailable observation evidence.

### Graph

The graph presents workflow Effect nodes and dependencies with readable,
Dagster-inspired layout. Selecting a node opens the shared detail pane.
Dynamic Task instances belong in Tasks, without invented workflow edges.

[Graph inspection field mapping](web-ui-graph-fields.md) traces each row
below to its existing state/service source and lists what is not yet
available, ahead of this section's own implementation task.

| Detail | Source and meaning |
| --- | --- |
| Structure | Resolvable workflow nodes, bindings, and depends_on |
| Result | Recorded lifecycle, setup/failure/cleanup timestamps, and error |
| Inputs | Persisted resolved input values, distinct from definition bindings |
| Outputs | Persisted outputs, not a fresh observation of external state; mutable outputs can change |
| Layers | Nested Effect inputs, locals, outputs, status, and error |
| Stored metadata | Existing scope, sequence, and other recorded fields |
| Events | Identifiable related events, otherwise a link to the session timeline |

The structure resolves against available configuration. A session's workflow
name does not constitute an immutable definition snapshot. Preserve unmatched
records and indicate missing configuration, missing data, or uncertain mapping.
A missing record is not automatically a failed node. Do not label a binding
from available configuration as the expression used in a past execution.

The graph does not promise historical run reconstruction, all setup/cleanup
attempts, arbitrary node reruns, or definition editing. Channels are delivery
wiring evaluated at delivery time, not ordinary node-build dependencies.
Keep them distinct from the main Effect graph. Effect inspection does not
invent Task-style `self.state`.

Status projections alone do not expose every stored input or nested layer.
The Go adapter provides these read projections from existing state contracts.

### Terminal

Use the existing Capture operation for read-only content, selection, and copy.
Refresh is manual. No input, attach, send_keys, automatic refresh, or capture
history is provided.

Capture returns session_name, task_id, and content. The UI labels its own
successful retrieval time as Fetched at, not the terminal's observation time.
On failure, preserve the displayed content and its previous retrieval time.

A plan permits at most one terminal-declaring Effect. A non-produced target
cannot be captured. A stopped session does not imply a persisted final capture.

### Shared details and resources

The right pane is closed initially. Selecting a Session, Task, or Node replaces
its content without losing the center view's draft or reading position.
Changing sessions must not leave another session's details visible.
Within one session, a Task can remain selected while the center shows Terminal.
Narrow screens use an overlay or temporary main-area detail view.

Session resource_id and Task resource are separate; neither implicitly fills
the other. HTTP(S) resources are links. Other identifiers remain visible and
copyable without a generic Open action. A remote server's filesystem path is
not a path on the user's desktop. Generic resource resolution, arbitrary URI
launching, previews, editing, and an Artifact model are outside the initial scope.

## Cross-session views

### Home

Home displays recent events from enumerable sessions, with explicit limits
and partial failures. Reuse existing per-session/subtree reads for a bounded
display aggregate. Do not invent a virtual session enclosing independent roots.

Deduplicate by event ID. An originating event and its parent notification
are distinct records even if their text matches. Show timestamp, receiving
session, type, summary, and origin when recorded.

Session and event-type filters operate on the retrieved scope. No all-time
counts, global full-history search, unbounded all-session stream, or inferred
progress percentage is promised. Older history uses the existing session or
subtree contract: ascending cursors support paging; descending v1 pages do
not imply infinite backward scrolling.

### Inbox

Inbox is an Escalations history filter over the same event retrieval mechanism:
`plect.tick.escalated` and `plect.terminal.escalate`. Show body, origin, receiver,
and recorded instance references, linking into the session workspace.

Escalation does not necessarily target a human. Terminal escalation moves one
parent hop; a parent may handle or forward it. No shared read/unread,
resolved/retracted, approval, or reply lifecycle is inferred from these records.
Opening an entry, emitting a message, or changing Task state does not resolve it.
Plugin-specific permission workflows do not define a universal Inbox contract.

## Implementation boundary

The [schema-contract decision](../adr/2026-09-06-web-api-schema-contract.md)
defines TypeSpec as the hand-edited HTTP contract source. OpenAPI and
Go/TypeScript API source are generated, committed, and checked for reproducibility.
Generated types stay within the Web boundary; handlers map them to existing
service arguments and results. Runtime validation and authentication remain
explicit. User-defined JSON is governed by existing configuration schemas.
Concrete generator and OpenAPI versions require the contract verification
described in that decision.


The browser uses React and TypeScript, built by Vite. Tailwind CSS and
shadcn/ui provide UI primitives. TanStack Query handles server data;
React Flow and ELK.js handle graph interaction and layout. Dependencies are
pinned at implementation time and retained in pnpm-lock.yaml. Use pnpm for
frontend dependency installation and scripts, with its version pinned in
package.json.

`plect-web` embeds static UI assets and serves JSON APIs and SSE from the
same origin. Users do not run a separate Node.js server. Go handlers call
existing services and read projections; CLI and UI share business semantics.
The event-bus socket and token remain server-side.

Connection origin, authentication, and stream recovery are centralized in
the client. Components contain neither hard-coded localhost URLs nor desktop
process calls. The initial Web UI connects to its own origin; a multi-server
management interface is not required.

Existing authentication and CSRF protection apply to JSON mutations. Loopback
is the default bind address; VPN access uses explicit bind/authentication
configuration.

The server owns execution state. After SSE activity, the client refreshes
affected status data as needed. It deduplicates events and follows existing
cursor semantics; unavailable replay triggers refetching. A stream does not
guarantee notification of every state change. Disconnected, stale, unavailable,
and empty are distinguishable states.

The HTTP API has a version and explicit compatibility checks for independently
updated clients and servers. This does not require speculative compatibility
shims. Paths, DTOs, errors, and recovery behavior are specified before their
consumers are implemented; internal Go structs are not automatically public DTOs.

## Desktop boundary

A desktop client shares the session UI and HTTP API:

- Local starts the necessary local server processes, checks readiness, and
  connects. Existing servers can be reused under an explicit ownership policy.
- Remote connects to a configured server without starting a local runtime.

Tauri 2 is the leading candidate; the framework remains undecided until a
small local/remote connection implementation validates it. Desktop responsibilities
are process ownership, connection credentials, and window/OS integration.
Authentication, origins, and SSE transport require verification; changing a
base URL alone does not constitute a desktop implementation.

Desktop planning covers supported OSes, signing/updates, port conflicts,
startup failure, existing-process identity, credential storage, and whether
owned servers outlive the window. Closing a window must not accidentally
stop sessions or someone else's server. Remote content receives no implicit
local OS privileges.

Bundling a server does not install or authenticate every agent CLI or satisfy
every workflow's runtime prerequisites. Installation automation and desktop
management screens are outside the first Web release.

## Delivery sequence and acceptance

| Slice | Scope | Acceptance |
| --- | --- | --- |
| Connection and reading | Go-served React UI, session tree, selected timeline, SSE | Real hierarchy and events; localhost/VPN access; reconnect/refetch; preserved UI state |
| Session operations | Create, Up/Down, user.emit, details, resources | Existing validation/errors; append versus delivery remains explicit |
| Inspection | Tasks, Graph, Terminal capture | Stored values versus configuration; missing data; lifecycle versus completion; capture failure |
| Cross-session awareness | Home and Escalations history | Bounded retrieval and partial failures; no invented unresolved-work model |
| Desktop feasibility | Local startup and remote connection | Shared UI/API, process ownership, authentication, shutdown policy |

Verification covers nested/independent/sibling sessions, notification origin
and receiver, all lifecycle and completion values, missing/mismatched graph
records, nested layers, distinct Session/Task resources, capture failure,
bounded cross-session data, and draft/selection/scroll preservation.

API contracts, retrieval bounds/frequency, build integration, and the Web UI
cutover are implementation planning details. The reference prototype is not
evidence that these real-data paths have been implemented.

## Semantic references

Plecture's language and state contracts are authoritative:

- [Workflows](../language/workflows.md), [Effects](../language/effects.md),
  [Tasks](../language/tasks.md), [Channels](../language/channels.md), and
  [Resource observers](../language/resource-observers.md).
- [State](../../contracts/state/state.go) and
  [event](../../contracts/event/event.go) contracts.
- [Session relationships](../../app/internal/domain/session.go),
  [Status](../../app/internal/service/status.go),
  [Task setup](../../app/internal/service/tasksetup.go),
  [workflow inspection](../../app/internal/service/workflow.go),
  [plan construction](../../app/internal/service/lifecycle_plan.go),
  [event reads](../../app/internal/service/event.go), and
  [Capture](../../app/internal/service/capture.go).

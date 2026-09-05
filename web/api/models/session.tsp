namespace Plecture.WebApi;

/// Whether any run-scoped task instance has produced — `service.RunState`
/// (`domain.RunState`). Distinct from `SessionHealthState`: a session can be
/// up and unhealthy, or down and never probed.
enum SessionRunState {
  up,
  down,
}

/// The declared-alive-probe evaluation (`domain.HealthState`), independent
/// of run state.
enum SessionHealthState {
  healthy,
  unhealthy,
  undeclared,
  stalled,
}

/// A session's self-reported free-text status line (`domain.Message`). Not
/// interpreted by the server; a slot for external updaters.
model SessionMessage {
  text: string;
  updatedAt: utcDateTime;
}

/// One run-scoped task instance's lifecycle summary
/// (`service.StatusRuntimeTask`). Not the Tasks tab's per-instance detail —
/// `status` is the bare lifecycle string, with no done_when/outputs/state.
model SessionTaskSummary {
  instance: string;
  status: string;
}

/// What a session is, independent of whether it is running right now —
/// `service.StatusIdentity`.
model SessionIdentity {
  sessionName: string;

  @doc("Absent and empty are equivalent for every optional field on this model: the API omits an unset value rather than emitting it as `null` or `\"\"`, mirroring the Go source's `omitempty` struct tags.")
  resourceId?: string;
  title?: string;
  branch?: string;
  workflow?: string;
  tag?: string;
  parentSession?: string;
  children?: string[];

  /// The workflow input values this session was created with
  /// (`contract.Session.Inputs`) — user-defined, workflow-schema-validated
  /// JSON that this API preserves rather than interprets.
  @doc("Arbitrary user-defined JSON: the workflow input values this session was created with. Preserved verbatim, not reinterpreted or schema-checked by this API.")
  inputs?: Record<unknown>;

  createdAt: utcDateTime;
}

/// Whether a session is actually alive right now, and its recent runtime
/// facts — `service.StatusRuntime`.
model SessionRuntime {
  run: SessionRunState;
  health?: SessionHealthState;
  lastCheckedAt?: utcDateTime;
  lastActivityAt?: utcDateTime;
  tasks?: SessionTaskSummary[];
  workspaceDirPath?: string;
  workspaceDirExists: boolean;
  message?: SessionMessage;
}

/// One row of the session list — `service.ListEntry`. A separate, smaller
/// projection from `SessionDetail`: the list is built for every session on
/// every poll, so it carries no per-session Tasks/Flow detail.
model SessionSummary {
  sessionName: string;
  title?: string;

  @doc("A human-readable status label combining run/health/message into one string for compact display; not a machine-readable status of its own (see `run` and `health`).")
  displayStatus: string;
  run: SessionRunState;
  health?: SessionHealthState;
  resourceId: string;
  lastActiveAt?: utcDateTime;
  message?: SessionMessage;
  branch?: string;
  parentSession?: string;
  tasks?: SessionTaskSummary[];
}

/// A single session's full identity and runtime facts, plus the
/// session-wide notices and destruction record `service.StatusResult`
/// carries alongside its Identity/Runtime layers. The Work (Tasks) and Flow
/// (event) layers are out of scope for this slice.
model SessionDetail extends SessionIdentity {
  ...SessionRuntime;
  warnings?: string[];
  destroyed?: boolean;
  destroyedAt?: utcDateTime;
}

package service

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// statusFlowLimit bounds the "flow" layer to the most recent events — status
// is a point-in-time snapshot, not a timeline (plect event list serves that).
const statusFlowLimit = 5

// StatusIdentity is layer 1: what this session is, independent of whether it
// is currently running. No workspace-provider-shaped field belongs here —
// ResourceID is an opaque string any workspace provider can own.
type StatusIdentity struct {
	SessionName   string                         `json:"session_name"`
	ResourceID    string                         `json:"resource_id,omitempty"`
	Title         string                         `json:"title,omitempty"`
	Branch        string                         `json:"branch,omitempty"`
	Workflow      string                         `json:"workflow,omitempty"`
	Population    *contract.PopulationProvenance `json:"population,omitempty"`
	Tag           string                         `json:"tag,omitempty"`
	ParentSession string                         `json:"parent_session,omitempty"`
	Children      []string                       `json:"children,omitempty"`
	// Inputs is the workflow input values this session was created with —
	// user-defined, workflow-schema-validated JSON preserved verbatim rather
	// than interpreted here.
	Inputs    map[string]any `json:"inputs,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// StatusRuntimeTask is one run-scoped task instance's lifecycle state.
type StatusRuntimeTask struct {
	Instance string `json:"instance"`
	Status   string `json:"status"`
}

// StatusRuntime is layer 2: whether the session is actually alive right now.
type StatusRuntime struct {
	Run                domain.RunState      `json:"run"`
	Health             domain.HealthState   `json:"health,omitempty"`
	LastCheckedAt      time.Time            `json:"last_checked_at,omitzero"`
	LastActivityAt     time.Time            `json:"last_activity_at,omitzero"`
	Tasks              []StatusRuntimeTask  `json:"tasks,omitempty"`
	WorkspaceDirPath   string               `json:"workspace_dir_path,omitempty"`
	WorkspaceDirExists bool                 `json:"workspace_dir_exists"`
	Conversation       *domain.Conversation `json:"conversation,omitempty"`
	Message            *domain.Message      `json:"message,omitempty"`
	AttachCommand      string               `json:"attach_command,omitempty"`
}

// StatusChain is one [[chains]] evaluation against a task instance's facts —
// the same dry-run report the retired `plect check` used to give.
type StatusChain struct {
	ChainID        string   `json:"chain_id"`
	Workflow       string   `json:"workflow,omitempty"`
	Fired          bool     `json:"fired"`
	BlockedReason  string   `json:"blocked_reason,omitempty"`
	MissingOutputs []string `json:"missing_outputs,omitempty"`
	AlreadyActive  bool     `json:"already_active,omitempty"`
	TargetSession  string   `json:"target_session,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

// StatusTask is layer 3: one task instance's work — its outputs (dynamic and
// mutable alike, rendered generically) and, when it declares a done_when, the
// gate's evaluation, heartbeat budget, and chain plan. Action/Summary/Body/
// ReviewerCommand/JudgeCommands/UnmetItems/Fingerprint carry the same
// decision-making material the retired `plect check` used to report per
// instance, so orchestrator consumers reading `plect status --json` lose
// nothing by that CLI's retirement.
type StatusTask struct {
	Instance string         `json:"instance"`
	TaskID   string         `json:"task_id,omitempty"`
	Scope    string         `json:"scope"`
	Status   string         `json:"status"`
	Dynamic  bool           `json:"dynamic,omitempty"`
	Name     string         `json:"name,omitempty"`
	Resource string         `json:"resource,omitempty"`
	Outputs  map[string]any `json:"outputs,omitempty"`
	// State is what this instance holds about itself, read by its completion
	// predicate as `self.state.*`.
	State map[string]any `json:"state,omitempty"`
	// Observed is the last observation of this instance's resource and when
	// it was taken. Reporting renders it rather than taking a new one, so the
	// timestamp is what tells a reader how old these facts are.
	Observed          *contract.ResourceObservation `json:"observed,omitempty"`
	DoneWhen          *task.DoneWhenResult          `json:"done_when,omitempty"`
	Action            string                        `json:"action,omitempty"` // satisfied|wait|review_required|kick|escalate
	HeartbeatTicks    int                           `json:"heartbeat_ticks,omitempty"`
	HeartbeatBudget   int                           `json:"heartbeat_budget,omitempty"`
	Summary           string                        `json:"summary,omitempty"`
	Body              string                        `json:"body,omitempty"`
	ReviewerCommand   string                        `json:"reviewer_command,omitempty"`
	JudgeCommands     []string                      `json:"judge_commands,omitempty"`
	UnmetItems        []CheckUnmetItem              `json:"unmet_items,omitempty"`
	Fingerprint       string                        `json:"fingerprint,omitempty"`
	Chains            []StatusChain                 `json:"chains,omitempty"`
	Finalized         bool                          `json:"finalized,omitempty"`
	PersistedDoneWhen *contract.DoneWhenState       `json:"persisted_done_when,omitempty"`
}

// StatusFlow is layer 4: the session's recent inbound/outbound traffic.
type StatusFlow struct {
	Events []event.Event `json:"events,omitempty"`
}

// StatusResult is the pure fact renderer `plect status` reports: four layers of
// state, no workspace-provider-shaped field among them.
type StatusResult struct {
	Identity StatusIdentity `json:"identity"`
	Runtime  StatusRuntime  `json:"runtime"`
	Work     []StatusTask   `json:"work,omitempty"`
	Flow     StatusFlow     `json:"flow"`
	// Warnings carries config-level notices unrelated to any one instance —
	// the same session-wide notices `plect check` used to report (e.g. a
	// surviving legacy chains/*.toml file).
	Warnings    []string  `json:"warnings,omitempty"`
	Destroyed   bool      `json:"destroyed,omitempty"`
	DestroyedAt time.Time `json:"destroyed_at,omitzero"`
}

// probeFaultWarnings renders each activity probe that failed to produce an
// envelope. A failed probe contributes no evidence at all, so without this it
// would be indistinguishable from a surface that is simply quiet.
func probeFaultWarnings(report HealthReport) []string {
	out := make([]string, 0, len(report.ProbeErrors))
	for _, pe := range report.ProbeErrors {
		line := fmt.Sprintf("%s: %s", pe.Instance, pe.Reason)
		if pe.Stderr != "" {
			line += ": " + pe.Stderr
		}
		out = append(out, line)
	}
	return out
}

// Status returns the four-layer fact report for a session. It reads persisted
// state only — pass refresh=true to re-fetch dynamic outputs from the source
// of truth first (mirrors the old `plect show --refresh`).
func Status(cfg *config.Config, store *state.Store, identifier string) (*StatusResult, error) {
	sessionName, session, err := resolveSession(cfg, store, identifier)
	if err != nil {
		if svcErr, ok := err.(*Error); ok && svcErr.Code == ErrSessionNotFound {
			if tomb, tombErr := lookupTombstone(cfg, store, identifier); tombErr == nil && tomb != nil {
				return tombstoneStatusResult(tomb), nil
			}
		}
		return nil, err
	}

	wtExists := fileExists(session.WorkspaceDirPath)
	runState := sessionRunState(cfg, session)
	healthReport, healthState := sessionHealthReport(cfg, store, sessionName)

	displayTitle := sessionDisplayTitle(cfg, session)

	_, _, computed, chainPlan, err := evaluateSessionActions(cfg, store, sessionName, false, "")
	if err != nil {
		return nil, err
	}
	warnings := probeFaultWarnings(healthReport)
	actionsByInstance := make(map[string]computedAction, len(computed))
	for _, c := range computed {
		actionsByInstance[c.instance] = c
	}
	chainsByInstance := make(map[string][]StatusChain, len(chainPlan))
	for _, sp := range chainPlan {
		chainsByInstance[sp.Instance] = append(chainsByInstance[sp.Instance], StatusChain{
			ChainID:        sp.ChainID,
			Workflow:       sp.Workflow,
			Fired:          sp.Fired,
			BlockedReason:  sp.BlockedReason,
			MissingOutputs: sp.MissingOutputs,
			AlreadyActive:  sp.AlreadyActive,
			TargetSession:  sp.TargetSession,
			Warnings:       sp.Warnings,
		})
	}

	sessions, err := store.AllE()
	if err != nil {
		return nil, err
	}

	events, err := EventRecent(cfg, store, sessionName, statusFlowLimit)
	if err != nil {
		return nil, err
	}

	return &StatusResult{
		Identity: StatusIdentity{
			SessionName:   sessionName,
			ResourceID:    identityResourceID(session),
			Title:         displayTitle,
			Branch:        session.Branch,
			Workflow:      session.Workflow,
			Population:    session.Population,
			Tag:           sessionTag(sessionName),
			ParentSession: session.ParentSession,
			Children:      childNames(sessions, sessionName),
			Inputs:        session.Inputs,
			CreatedAt:     session.CreatedAt,
		},
		Runtime: StatusRuntime{
			Run:                runState,
			Health:             healthState,
			LastCheckedAt:      healthReport.LastCheckedAt,
			LastActivityAt:     healthReport.LastActivityAt,
			Tasks:              runtimeTaskViews(session),
			WorkspaceDirPath:   session.WorkspaceDirPath,
			WorkspaceDirExists: wtExists,
			Conversation:       session.Conversation,
			Message:            session.Message,
			AttachCommand:      attachCommandFor(cfg, session),
		},
		Work:     statusTaskViews(cfg, loadDisplayTasks(cfg), session, sessions, actionsByInstance, chainsByInstance),
		Flow:     StatusFlow{Events: events},
		Warnings: warnings,
	}, nil
}

// attachCommandFor resolves the declared attach command for display, mirroring
// Attach's lookup but degrading to "" (no attach target, or not yet produced)
// instead of an error — `plect status` reports facts, it doesn't fail on them.
func attachCommandFor(cfg *config.Config, session *domain.Session) string {
	plan, err := buildPlanForSession(cfg, session.WorkspaceDirPath, session)
	if err != nil {
		return ""
	}
	target := plan.TerminalTask()
	if target == nil {
		return ""
	}
	st, ok := session.Tasks[target.NodeID]
	if !ok || st == nil || st.Status != contract.TaskStatusProduced {
		return ""
	}
	// This is a display string nobody runs, so the run directory a shell
	// attach verb materializes into is removed as soon as it is read.
	dir, err := os.MkdirTemp("", "plect-attach-display-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	cmdStr, err := task.TerminalCommand(terminalBinding(plan, session), "attach", sessionVars(cfg, session, plan), dir)
	if err != nil {
		return ""
	}
	return cmdStr
}

// sessionDisplayTitle resolves the session's display title from the
// workflow's [display] templates, without pulling the display status line
// alongside it.
func sessionDisplayTitle(cfg *config.Config, session *domain.Session) string {
	var cached cachedInfo
	applyDisplay(loadDisplayWorkflows(cfg), session, &cached)
	return cached.Title
}

// runtimeTaskViews projects the session's run-scoped task instances — the
// "run-scoped task produced state" layer-2 fact.
func runtimeTaskViews(session *domain.Session) []StatusRuntimeTask {
	var out []StatusRuntimeTask
	for _, key := range sortedTaskKeys(session.Tasks) {
		st := session.Tasks[key]
		if st == nil || st.Scope != contract.TaskScopeRun || key == contract.WorkflowPseudoNodeID {
			continue
		}
		out = append(out, StatusRuntimeTask{Instance: key, Status: st.Status})
	}
	return out
}

// statusTaskViews projects a session's task instances for the "work" layer:
// outputs (dynamic and mutable rendered identically), the done_when
// evaluation, its heartbeat budget, and its chain plan. actions supplies the
// already-evaluated done_when result per produced instance (from
// evaluateSessionActions, the same evaluation `plect check`/`plect tick` act on),
// so sessionTaskItems reuses it instead of evaluating done_when a second time.
func statusTaskViews(cfg *config.Config, declarations taskDeclarations, session *domain.Session, sessions map[string]*domain.Session, actions map[string]computedAction, chains map[string][]StatusChain) []StatusTask {
	cached := make(map[string]task.DoneWhenResult, len(actions))
	for key, a := range actions {
		cached[key] = a.result
	}
	items := sessionTaskItems(cfg, declarations, session, sessions, cached)
	if items == nil {
		return nil
	}
	out := make([]StatusTask, len(items))
	for i, it := range items {
		v := StatusTask{
			Instance:  it.instance,
			TaskID:    it.taskID,
			Scope:     it.scope,
			Status:    it.status,
			Dynamic:   it.dynamic,
			Name:      it.name,
			Resource:  it.resource,
			Outputs:   it.outputs,
			State:     it.state,
			Observed:  it.observed,
			DoneWhen:  it.doneWhen,
			Chains:    chains[it.instance],
			Finalized: it.finalized,
		}
		if a, ok := actions[it.instance]; ok {
			v.Action = a.action.Action
			v.HeartbeatTicks = a.action.HeartbeatTicks
			v.HeartbeatBudget = a.action.HeartbeatBudget
			v.Summary = a.action.Summary
			v.Body = a.action.Body
			v.ReviewerCommand = a.action.ReviewerCommand
			v.JudgeCommands = a.action.JudgeCommands
			v.UnmetItems = a.action.UnmetItems
			v.Fingerprint = a.action.Fingerprint
		}
		out[i] = v
	}
	return out
}

// tombstoneStatusResult projects a destroyed session's tombstone into the
// minimal StatusResult a destroyed session can still offer: identity plus
// each task instance's final outputs/done_when state as recorded at destroy.
func tombstoneStatusResult(tomb *contract.Tombstone) *StatusResult {
	var work []StatusTask
	for key, st := range tomb.Tasks {
		if st == nil {
			continue
		}
		work = append(work, StatusTask{
			Instance:          key,
			TaskID:            st.TaskID,
			Scope:             st.Scope,
			Status:            st.Status,
			Dynamic:           st.Dynamic,
			Name:              st.Name,
			Resource:          st.Resource,
			Outputs:           st.Outputs,
			State:             st.State,
			Observed:          st.Observed,
			Finalized:         !st.FinalizedAt.IsZero(),
			PersistedDoneWhen: st.DoneWhen,
		})
	}
	slices.SortFunc(work, func(a, b StatusTask) int { return strings.Compare(a.Instance, b.Instance) })
	return &StatusResult{
		Identity: StatusIdentity{
			SessionName:   tomb.Name,
			ResourceID:    identityResourceID(&tomb.Session),
			Workflow:      tomb.Workflow,
			Tag:           sessionTag(tomb.Name),
			ParentSession: tomb.ParentSession,
			CreatedAt:     tomb.CreatedAt,
		},
		Work:        work,
		Destroyed:   true,
		DestroyedAt: tomb.DestroyedAt,
	}
}

// identityResourceID is the session's canonical resource identifier.
func identityResourceID(s *domain.Session) string {
	return s.ResourceID
}

// sessionTag extracts the session-identity tag from a session name's
// "<resource>+<tag>" convention (effectiveTag, chainSpawnTag) — provider-
// agnostic string parsing over a name plect itself produced.
func sessionTag(name string) string {
	if idx := strings.LastIndex(name, "+"); idx >= 0 {
		return name[idx+1:]
	}
	return ""
}

// StatusSummary is the default `plect status --json` shape: identity plus, per
// done_when-bearing instance only, the material an orchestrator needs to pick
// its next action — no full outputs/flow/runtime dump (that's --full).
type StatusSummary struct {
	Identity StatusSummaryIdentity `json:"identity"`
	Work     []StatusSummaryWork   `json:"work,omitempty"`
}

type StatusSummaryIdentity struct {
	SessionName   string                         `json:"session_name"`
	ResourceID    string                         `json:"resource_id,omitempty"`
	Workflow      string                         `json:"workflow,omitempty"`
	Population    *contract.PopulationProvenance `json:"population,omitempty"`
	Tag           string                         `json:"tag,omitempty"`
	ParentSession string                         `json:"parent_session,omitempty"`
}

type StatusSummaryWork struct {
	Instance        string                 `json:"instance"`
	Action          string                 `json:"action,omitempty"`
	HeartbeatBudget string                 `json:"heartbeat_budget,omitempty"`
	DoneWhen        *StatusSummaryDoneWhen `json:"done_when,omitempty"`
	Chains          []StatusSummaryChain   `json:"chains,omitempty"`
	JudgeCommands   []string               `json:"judge_commands,omitempty"`
}

type StatusSummaryDoneWhen struct {
	Overall task.DoneStatus         `json:"overall"`
	Leaves  []StatusSummaryDoneLeaf `json:"leaves,omitempty"`
}

// StatusSummaryDoneLeaf carries only what a leaf's line in the text renderer
// shows: the referenced-and-observed value, never the instance's full outputs
// map (a done_when expression that doesn't read an output never surfaces it).
type StatusSummaryDoneLeaf struct {
	Kind     string          `json:"kind"`
	Name     string          `json:"name,omitempty"`
	Expr     string          `json:"expr"`
	Status   task.DoneStatus `json:"status"`
	Output   string          `json:"output,omitempty"`
	Value    string          `json:"value,omitempty"`
	Revision string          `json:"revision,omitempty"`
}

type StatusSummaryChain struct {
	ChainID        string   `json:"chain_id"`
	Workflow       string   `json:"workflow,omitempty"`
	Fired          bool     `json:"fired"`
	AlreadyActive  bool     `json:"already_active,omitempty"`
	TargetSession  string   `json:"target_session,omitempty"`
	BlockedReason  string   `json:"blocked_reason,omitempty"`
	MissingOutputs []string `json:"missing_outputs,omitempty"`
}

// Summarize projects a StatusResult into the default `--json` shape: only
// instances that declare a done_when (the same filter the text renderer's
// "Done when" section applies), and only the leaf/chain fields an
// orchestrator's next-action decision needs.
func Summarize(result *StatusResult) *StatusSummary {
	sum := &StatusSummary{
		Identity: StatusSummaryIdentity{
			SessionName:   result.Identity.SessionName,
			ResourceID:    result.Identity.ResourceID,
			Workflow:      result.Identity.Workflow,
			Population:    result.Identity.Population,
			Tag:           result.Identity.Tag,
			ParentSession: result.Identity.ParentSession,
		},
	}
	for _, t := range result.Work {
		if t.DoneWhen == nil {
			continue
		}
		w := StatusSummaryWork{
			Instance:        t.Instance,
			Action:          t.Action,
			HeartbeatBudget: HeartbeatBudgetString(t.HeartbeatTicks, t.HeartbeatBudget),
			JudgeCommands:   t.JudgeCommands,
		}
		dw := &StatusSummaryDoneWhen{Overall: t.DoneWhen.Overall}
		for _, leaf := range t.DoneWhen.Leaves {
			dw.Leaves = append(dw.Leaves, summarizeDoneLeaf(leaf))
		}
		w.DoneWhen = dw
		for _, c := range t.Chains {
			w.Chains = append(w.Chains, StatusSummaryChain{
				ChainID:        c.ChainID,
				Workflow:       c.Workflow,
				Fired:          c.Fired,
				AlreadyActive:  c.AlreadyActive,
				TargetSession:  c.TargetSession,
				BlockedReason:  c.BlockedReason,
				MissingOutputs: c.MissingOutputs,
			})
		}
		sum.Work = append(sum.Work, w)
	}
	return sum
}

func summarizeDoneLeaf(leaf task.DoneLeafResult) StatusSummaryDoneLeaf {
	out := StatusSummaryDoneLeaf{
		Kind:   leaf.Kind,
		Expr:   leaf.Expr,
		Status: leaf.Status,
	}
	if leaf.Kind == "judge" {
		out.Name = leaf.ID
		if leaf.CurrentRevision != "" {
			out.Revision = leaf.CurrentRevision
		} else {
			out.Revision = leaf.Revision
		}
		return out
	}
	out.Output = leaf.Output
	if leaf.Observed {
		out.Value = leaf.Value
	}
	return out
}

// HeartbeatBudgetString renders a heartbeat budget as "ticks/budget", or just
// "ticks" when unbounded; "" when no heartbeat tick has been consumed yet.
func HeartbeatBudgetString(ticks, budget int) string {
	switch {
	case budget > 0:
		return fmt.Sprintf("%d/%d", ticks, budget)
	case ticks > 0:
		return fmt.Sprintf("%d", ticks)
	default:
		return ""
	}
}

// DoneWhenCell renders the `plect ls` DONE_WHEN column: "-" when no instance
// declares a done_when, the instance's own compact rendering when there is
// exactly one, and the worst symbol's per-status counts when there are
// several (worst order: unsatisfied > pending > satisfied).
func DoneWhenCell(views []TaskInstanceView) string {
	var results []*task.DoneWhenResult
	for _, v := range views {
		if v.DoneWhen != nil {
			results = append(results, v.DoneWhen)
		}
	}
	switch len(results) {
	case 0:
		return "-"
	case 1:
		return doneWhenCellOne(results[0])
	default:
		counts := map[task.DoneStatus]int{}
		for _, r := range results {
			counts[r.Overall]++
		}
		var parts []string
		for _, st := range []task.DoneStatus{task.DoneUnsatisfied, task.DonePending, task.DoneSatisfied} {
			if n := counts[st]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s%d", doneSymbol(st), n))
			}
		}
		return strings.Join(parts, " ")
	}
}

func doneWhenCellOne(dw *task.DoneWhenResult) string {
	satisfied := 0
	for _, l := range dw.Leaves {
		if l.Status == task.DoneSatisfied {
			satisfied++
		}
	}
	return fmt.Sprintf("%s %d/%d", doneSymbol(dw.Overall), satisfied, len(dw.Leaves))
}

func doneSymbol(s task.DoneStatus) string {
	switch s {
	case task.DoneSatisfied:
		return "✓"
	case task.DoneUnsatisfied:
		return "✗"
	default:
		return "⋯"
	}
}

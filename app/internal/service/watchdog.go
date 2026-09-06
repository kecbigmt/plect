package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// HealthReport is the outcome of one health-cycle observation.
type HealthReport struct {
	SessionName string `json:"session_name"`
	Healthy     bool   `json:"healthy"`
	// Declared reports whether any produced run-scoped task instance declares
	// an alive probe at all — distinguishes "ran and passed" from "nothing to
	// evaluate" (State() surfaces the latter as HealthUndeclared).
	Declared bool `json:"declared"`
	// ActivityDue reports whether unmet run-scoped work exists that this
	// session is expected to act on next, derived from declared
	// done_when/task state, never from Message. This is the accusation side
	// of a stall: an activity probe can pardon silence but never raise it.
	ActivityDue bool `json:"activity_due,omitempty"`
	// ActivityDeclared reports whether at least one produced run-scoped
	// task instance ran an activity probe that produced an envelope. False
	// when no activity probe is declared at all, and when every declared one
	// reports no basis (or fails to produce a usable envelope) — both read
	// as "no basis to judge activity."
	ActivityDeclared bool `json:"activity_declared,omitempty"`
	// ActivityFresh reports whether any contributing activity probe reported
	// evidence within the stall threshold. Meaningless unless
	// ActivityDeclared and ActivityDue are both true.
	ActivityFresh bool `json:"activity_fresh,omitempty"`
	// ProbeErrors names the declared activity probes that failed to produce
	// an envelope this cycle. They contribute no evidence, exactly like a
	// probe reporting no basis, so without this list a persistently broken
	// probe would be indistinguishable from a quiet one.
	ProbeErrors         []ProbeError `json:"probe_errors,omitempty"`
	Reason              string       `json:"reason,omitempty"`
	LastCheckedAt       time.Time    `json:"last_checked_at,omitzero"`
	LastActivityAt      time.Time    `json:"last_activity_at,omitzero"`
	ActivityFingerprint string       `json:"-"`
	Pushed              bool         `json:"pushed,omitempty"`
	PushTarget          string       `json:"push_target,omitempty"`
	// WakeWarning is set when the dead event was recorded on PushTarget but
	// best-effort waking it (Up) failed — the push itself still succeeded.
	WakeWarning string `json:"wake_warning,omitempty"`
}

// ProbeError is one declared activity probe that failed to contribute this
// cycle. ExitCode and Stderr are populated only when the command itself failed
// to run to completion; a probe that ran and printed an unusable envelope
// carries Reason alone.
type ProbeError struct {
	Instance string `json:"instance"`
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Reason   string `json:"reason"`
}

type HealthcheckParams struct {
	SessionName string
	Config      config.HealthcheckConfig
}

// State projects the report to the four-value display fact `plect status`
// reports.
//
//   - Undeclared beats everything else when there is no declared alive probe
//     to evaluate at all and activity has never once been observed for this
//     session.
//   - Unhealthy beats activity: a failing alive probe is reported as such
//     regardless of activity evidence.
//   - When no activity is due, a passing alive probe is healthy outright.
//   - When activity is due but no activity probe contributes this tick and
//     none has ever been observed, there is no basis to call it either
//     healthy or stalled.
//   - Otherwise, fresh activity evidence is healthy and its absence is
//     stalled — once activity has been observed at least once, a probe
//     dying afterward is a stall, not a reversion to undeclared: the
//     basis to judge (LastActivityAt) survives the probe that produced it.
func (r HealthReport) State() domain.HealthState {
	everObserved := !r.LastActivityAt.IsZero()
	if !r.Declared && !r.ActivityDeclared && !everObserved {
		return domain.HealthUndeclared
	}
	if !r.Healthy {
		return domain.HealthUnhealthy
	}
	if !r.ActivityDue {
		return domain.HealthHealthy
	}
	if !r.ActivityDeclared && !everObserved {
		return domain.HealthUndeclared
	}
	if r.ActivityFresh {
		return domain.HealthHealthy
	}
	return domain.HealthStalled
}

// EvaluateHealth composes health over every run-scoped node the session's
// frozen workflow currently declares — not only the produced subset. A
// produced node runs its declared `[health]` probes; a failed or missing
// node is a structural failure, reported unhealthy with a reason naming the
// node. This composition is only evaluated for a session with at least one
// produced run-scoped task: a session with none (already `plect down`, or a
// purely session-scoped resource) has no health verdict at all — L2 only
// detects a process/runtime/subscription that died while it was supposed to
// be up, not a session that was deliberately brought down. The first
// failing probe, whether an alive probe or a structural failure, wins;
// Reason names the failing node.
func EvaluateHealth(cfg *config.Config, store *state.Store, name string) (HealthReport, error) {
	s, err := store.GetE(name)
	if err != nil {
		return HealthReport{}, err
	}
	if s == nil {
		return HealthReport{}, &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("session %q not found", name)}
	}
	docs, defs, err := cfg.LoadTaskDeclarations(s.WorkspaceDirPath)
	if err != nil {
		return HealthReport{}, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load task declarations: %v", err)}
	}
	// A session's frozen workflow failing to resolve is this session's own
	// plan-resolution failure, not a silent absence of a plan: unlike
	// sessionWorkflowConfig (used where a best-effort address lookup is
	// fine), a session naming a workflow that no longer exists must surface
	// as an error here so one broken workflow reports as one session's
	// health-evaluation failure rather than a falsely healthy verdict.
	wf, wfErr := cfg.ResolveSessionWorkflow(s)
	if wfErr != nil {
		return HealthReport{}, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("resolve workflow: %v", wfErr)}
	}
	healthCfg := config.DefaultHealthcheckConfig()
	if wf != nil {
		healthCfg = config.NormalizeHealthcheckConfig(wf.Healthcheck)
	}
	nodes := nodeAddresses(cfg, s)
	plan, err := healthTerminalPlan(s.Tasks, nodes, defs)
	if err != nil {
		return HealthReport{}, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("resolve terminal binding: %v", err)}
	}
	now := time.Now()
	// cfg.RunScopeUp is the one shared authority for "is this session's
	// current plan up" (reactor, dispatch, and population capacity all go
	// through it too), so the structural gate below defers to it rather than
	// deriving its own answer.
	report := evaluateHealthFor(name, cfg.RunScopeUp(s), s.Tasks, docs, defs, nodes, sessionVars(cfg, s, plan), healthCfg.StallThreshold.Duration, s.Health, now)
	finalizeActivityObservation(&report, s.Health, healthCfg.StallThreshold.Duration, now)
	persistHealthState(store, name, report, now)
	return report, nil
}

func sessionWorkflowConfig(cfg *config.Config, workflowID, workspaceDirPath string) *config.WorkflowFile {
	workflows, err := cfg.LoadWorkflows(workspaceDirPath)
	if err != nil {
		return nil
	}
	wf, ok := workflows[workflowID]
	if !ok {
		return nil
	}
	return &wf
}

// currentPlanRunScopedNodeIDs returns every run-scoped node the session's
// frozen workflow currently declares, sorted for a deterministic
// first-failure report. A node the workflow no longer declares is absent
// from this list regardless of what session.Tasks still holds for it, which
// is what makes a stale task entry inert to health.
func currentPlanRunScopedNodeIDs(nodes map[string]string, defs map[string]config.TaskDefinition) []string {
	var ids []string
	for id, address := range nodes {
		def, ok := defs[address]
		if !ok || def.Scope != config.TaskScopeRun {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func healthTerminalPlan(tasks map[string]*contract.TaskState, nodes map[string]string, defs map[string]config.TaskDefinition) (*task.Plan, error) {
	var terminal *task.Resolved
	for _, key := range sortedTaskKeys(tasks) {
		st := tasks[key]
		if st == nil || st.Status != contract.TaskStatusProduced || st.Dynamic {
			continue
		}
		address, ok := nodes[key]
		if !ok {
			continue
		}
		def, ok := defs[address]
		if !ok {
			continue
		}
		resolved, err := task.ResolveDefinition(def, key)
		if err != nil {
			continue
		}
		if resolved.Terminal == nil {
			continue
		}
		if terminal != nil {
			return nil, fmt.Errorf("more than one produced workflow node declares [terminal]: %q and %q", terminal.NodeID, resolved.NodeID)
		}
		terminal = &resolved
	}
	if terminal == nil {
		return nil, nil
	}
	plan := &task.Plan{}
	if terminal.Scope == config.TaskScopeSession {
		plan.Session = []task.Resolved{*terminal}
	} else {
		plan.Run = []task.Resolved{*terminal}
	}
	return plan, nil
}

func persistHealthState(store *state.Store, name string, report HealthReport, checkedAt time.Time) {
	err := store.Update(name, func(s *domain.Session) error {
		prev := s.Health
		if prev == nil {
			prev = &contract.HealthState{}
		}
		fingerprint := report.ActivityFingerprint
		lastActivityAt := prev.LastActivityAt
		if report.ActivityDeclared && fingerprint != "" && fingerprint != prev.LastFingerprint {
			lastActivityAt = checkedAt
		}
		stateText := string(report.State())
		s.Health = &contract.HealthState{
			LastCheckedAt:   checkedAt,
			LastActivityAt:  lastActivityAt,
			LastFingerprint: fingerprint,
			LastState:       stateText,
			LastReason:      reportReason(report),
			LastNotifiedAt:  prev.LastNotifiedAt,
			NotifyCount:     prev.NotifyCount,
		}
		s.UpdatedAt = checkedAt
		return nil
	})
	if err != nil {
		slog.Warn("persist health state failed", "session", name, "error", err)
	}
}

func reportReason(report HealthReport) string {
	if report.State() == domain.HealthUnhealthy {
		return report.Reason
	}
	return ""
}

// sessionOwesProgress reports whether any live task-document instance's
// completion predicate is unsatisfied — the work this session is expected to
// act on next. It reads declared state only, never a free-text message.
func sessionOwesProgress(tasks map[string]*contract.TaskState, docs map[string]config.TaskDocument) bool {
	for _, key := range sortedTaskKeys(tasks) {
		st := tasks[key]
		if st == nil || st.Status != contract.TaskStatusProduced {
			continue
		}
		doc, ok := docs[taskIDForInstance(key, st)]
		if !ok || doc.DoneWhen == nil {
			continue
		}
		if task.EvaluateTaskDoneWhen(doc.DoneWhen, documentCompletionState(st)).Overall != task.DoneSatisfied {
			return true
		}
	}
	return false
}

// evaluateHealthFor is EvaluateHealth's pure core, taking already-loaded task
// state/defs instead of fetching them from store/cfg so callers that already
// hold that data avoid a redundant load. gateOpen is cfg.RunScopeUp's answer
// for this session, computed once by the caller: the same shared authority
// every run-fact consumer defers to, so the structural composition below
// cannot disagree with it.
func evaluateHealthFor(name string, gateOpen bool, tasks map[string]*contract.TaskState, docs map[string]config.TaskDocument, defs map[string]config.TaskDefinition, nodes map[string]string, vars task.SessionVars, stallThreshold time.Duration, prev *contract.HealthState, now time.Time) HealthReport {
	declared := false
	// The accusation side of a stall is unmet work, and work is what a task
	// document declares. The probes that can pardon silence are a run-scoped
	// effect's, so due is decided across the session's documents and lowered
	// inside the probe walk below.
	activityDue := sessionOwesProgress(tasks, docs)
	activityDeclared := false
	activityFingerprintParts := []string{}
	probeErrors := []ProbeError{}

	// The structural composition — a failed or missing current-plan node is
	// itself an AND failure — only applies once the session has actually come
	// up. Nothing in the current plan is missing yet for a session that never
	// produced any run-scoped node: an aborted first-node `up`, or a session
	// that was deliberately brought down, reads as no verdict rather than
	// unhealthy.
	if gateOpen {
		currentPlan := currentPlanRunScopedNodeIDs(nodes, defs)
		for _, nodeID := range currentPlan {
			st := tasks[nodeID]
			switch {
			case st == nil:
				return HealthReport{SessionName: name, Healthy: false, Declared: true, ProbeErrors: probeErrors, Reason: fmt.Sprintf("%s: missing from the current plan", nodeID), LastCheckedAt: now}
			case st.Status == contract.TaskStatusCleaned:
				// Deliberately torn down, not failed or missing: contributes
				// nothing.
				continue
			case st.Status == contract.TaskStatusFailed:
				// A node whose [health].alive is a noop has no liveness probe
				// to run, so this structural check is the only place a failed
				// noop node's failure reaches the health report.
				errText := st.Error
				if errText == "" {
					errText = "setup failed"
				}
				return HealthReport{SessionName: name, Healthy: false, Declared: true, ProbeErrors: probeErrors, Reason: fmt.Sprintf("%s: %s", nodeID, errText), LastCheckedAt: now}
			case st.Status != contract.TaskStatusProduced:
				continue
			}

			def := defs[instanceDefinitionAddress(nodeID, st, nodes)]
			comp, compErr := composeInstance(def, st, vars)
			if compErr != nil {
				probeErrors = append(probeErrors, ProbeError{Instance: nodeID, Reason: compErr.Error()})
				continue
			}

			// alive composes by AND and activity by OR across the layers of a
			// chain exactly as they compose across instances, so both fall out of
			// walking one flat target list.
			for _, target := range probeTargets(nodeID, def, st, comp) {
				if target.Alive != nil {
					declared = true
					if aliveErr := task.RunAliveProbe(context.Background(), target.probe(target.Alive), vars); aliveErr != nil {
						return HealthReport{SessionName: name, Healthy: false, Declared: true, ProbeErrors: probeErrors, Reason: fmt.Sprintf("%s: %v", target.Label, aliveErr), LastCheckedAt: now}
					}
				}
				if target.Activity == nil {
					continue
				}
				sig, sigErr := task.RunActivityProbe(context.Background(), target.probe(target.Activity), vars)
				switch {
				case sigErr != nil:
					probeErrors = append(probeErrors, activityProbeError(target.Label, target.Activity.Source(), sigErr))
				case sig != nil:
					activityDeclared = true
					// A probe may only lower this instance's expectation: no
					// envelope can manufacture an expectation done_when does not
					// already see.
					if sig.SilenceExpected {
						activityDue = false
					}
					activityFingerprintParts = append(activityFingerprintParts, target.Label+":"+sig.Fingerprint)
				}
			}
		}
	}

	report := HealthReport{
		SessionName:      name,
		Healthy:          true,
		Declared:         declared,
		ActivityDue:      activityDue,
		ActivityDeclared: activityDeclared,
		LastCheckedAt:    now,
	}
	if len(probeErrors) > 0 {
		report.ProbeErrors = probeErrors
	}
	// Every declaring instance's fingerprint contributes: activity composes
	// by OR, so a change anywhere in the session is evidence the session is
	// moving. Sorting keeps the composite stable against map iteration order.
	if len(activityFingerprintParts) > 0 {
		slices.Sort(activityFingerprintParts)
		report.ActivityFingerprint = strings.Join(activityFingerprintParts, "\x00")
	}
	return report
}

func activityProbeError(instance, cmd string, err error) ProbeError {
	entry := ProbeError{Instance: instance, Command: cmd, Reason: err.Error()}
	var execErr *task.ActivityProbeExecError
	if errors.As(err, &execErr) {
		entry.ExitCode = execErr.ExitCode
		entry.Stderr = stderrDigest(execErr.Stderr)
	}
	return entry
}

// probeStderrDigestLimit bounds how much of a runaway probe's stderr the
// report carries. The head is what survives, because a script's first error
// line is the one that names the cause.
const probeStderrDigestLimit = 512

func stderrDigest(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if len(stderr) <= probeStderrDigestLimit {
		return stderr
	}
	return strings.ToValidUTF8(stderr[:probeStderrDigestLimit], "") + "..."
}

func finalizeActivityObservation(report *HealthReport, prev *contract.HealthState, stallThreshold time.Duration, now time.Time) {
	lastActivityAt := time.Time{}
	lastFingerprint := ""
	if prev != nil {
		lastActivityAt = prev.LastActivityAt
		lastFingerprint = prev.LastFingerprint
	}
	if report.ActivityDeclared && report.ActivityFingerprint != "" && report.ActivityFingerprint != lastFingerprint {
		lastActivityAt = now
	}
	report.LastActivityAt = lastActivityAt
	// A probe that never contributed evidence (this tick or ever) has no
	// basis to judge freshness at all, so it defaults true rather than
	// false — but once LastActivityAt is on record, that basis survives the
	// current tick's probe dying, and freshness must be judged against it
	// rather than defaulted.
	if !report.ActivityDue || (!report.ActivityDeclared && lastActivityAt.IsZero()) {
		report.ActivityFresh = true
		return
	}
	if !lastActivityAt.IsZero() && now.Sub(lastActivityAt) <= stallThreshold {
		report.ActivityFresh = true
	}
}

func HealthcheckSession(cfg *config.Config, store *state.Store, params HealthcheckParams) (*HealthReport, error) {
	healthCfg := config.NormalizeHealthcheckConfig(&params.Config)
	before, err := store.GetE(params.SessionName)
	if err != nil {
		return nil, err
	}
	// A down session is skipped rather than probed and re-verdicted, mirroring
	// the gate CheckHeartbeatDeadman already applies internally. An unknown
	// session still falls through to EvaluateHealth below, which is what
	// reports ErrSessionNotFound.
	if before != nil && !cfg.RunScopeUp(before) {
		return &HealthReport{SessionName: params.SessionName}, nil
	}
	var prev *contract.HealthState
	if before != nil && before.Health != nil {
		cp := *before.Health
		prev = &cp
	}
	report, err := EvaluateHealth(cfg, store, params.SessionName)
	if err != nil {
		return nil, err
	}
	stateText := string(report.State())
	if stateText != string(domain.HealthUnhealthy) && stateText != string(domain.HealthStalled) {
		return &report, nil
	}
	notify, notifyCount := shouldNotifyHealth(prev, stateText, healthCfg)
	if !notify {
		return &report, nil
	}
	report.Pushed = pushHealthEscalation(cfg, store, params.SessionName, &report, notifyCount)
	if report.Pushed {
		persistHealthNotification(store, params.SessionName, notifyCount)
	}
	return &report, nil
}

func shouldNotifyHealth(prev *contract.HealthState, stateText string, healthCfg config.HealthcheckConfig) (bool, int) {
	if prev == nil {
		return true, 1
	}
	if prev.LastState != stateText {
		return true, prev.NotifyCount + 1
	}
	if prev.LastNotifiedAt.IsZero() {
		return true, prev.NotifyCount + 1
	}
	renotifyEvery := healthCfg.RenotifyEvery
	if renotifyEvery <= 0 {
		renotifyEvery = config.DefaultHealthcheckConfig().RenotifyEvery
	}
	period := healthCfg.Period.Duration
	if period <= 0 {
		period = config.DefaultHealthcheckConfig().Period.Duration
	}
	if time.Since(prev.LastNotifiedAt) >= time.Duration(renotifyEvery)*period {
		return true, prev.NotifyCount + 1
	}
	return false, prev.NotifyCount
}

func pushHealthEscalation(cfg *config.Config, store *state.Store, origin string, report *HealthReport, notifyCount int) bool {
	stateText := string(report.State())
	meta := map[string]string{
		"escalation_kind": "health." + stateText,
		"health_state":    stateText,
	}
	if !report.LastCheckedAt.IsZero() {
		meta["last_checked_at"] = report.LastCheckedAt.UTC().Format(time.RFC3339)
	}
	if !report.LastActivityAt.IsZero() {
		meta["last_activity_at"] = report.LastActivityAt.UTC().Format(time.RFC3339)
	}
	id, wakeErr, err := publishHealthEscalationToLiveAncestor(cfg, store, origin, TerminalParams{
		Type:     event.TypeTerminalEscalate,
		Summary:  fmt.Sprintf("%s healthcheck is %s", origin, stateText),
		Body:     healthEscalationBody(origin, report),
		Metadata: meta,
		DedupKey: fmt.Sprintf("%s|health|%s|%d", origin, stateText, notifyCount),
	})
	if err != nil {
		slog.Warn("publish health escalation failed", "session", origin, "error", err)
		return false
	}
	if id == "" {
		localID, _, localErr := publishTerminalTo(cfg, store, origin, origin, false, TerminalParams{
			Type:     event.TypeTerminalDead,
			Summary:  fmt.Sprintf("%s health escalation is undeliverable", origin),
			Body:     healthEscalationBody(origin, report),
			Metadata: meta,
			DedupKey: fmt.Sprintf("%s|health|%s|%d|undeliverable", origin, stateText, notifyCount),
		})
		if localErr != nil {
			slog.Warn("record undeliverable health escalation failed", "session", origin, "error", localErr)
			return false
		}
		if localID == "" {
			return false
		}
		report.PushTarget = origin
		return true
	}
	if wakeErr != nil {
		report.WakeWarning = wakeErr.Error()
	}
	report.PushTarget = id
	return true
}

func publishHealthEscalationToLiveAncestor(cfg *config.Config, store *state.Store, origin string, p TerminalParams) (target string, wakeErr error, err error) {
	visited := map[string]bool{origin: true}
	current := origin
	for {
		s, err := store.GetE(current)
		if err != nil {
			return "", nil, err
		}
		if s == nil {
			return "", nil, &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("session %q not found", current)}
		}
		if s.ParentSession == "" {
			return "", nil, nil
		}
		parent := resolveTerminalTarget(s.ParentSession)
		if parent == "" || visited[parent] {
			return "", nil, nil
		}
		visited[parent] = true
		parentHealth, healthErr := EvaluateHealth(cfg, store, parent)
		if healthErr != nil || parentHealth.State() == domain.HealthUnhealthy || parentHealth.State() == domain.HealthStalled {
			current = parent
			continue
		}
		storedID, wakeErr, publishErr := publishTerminalTo(cfg, store, origin, parent, true, p)
		if publishErr != nil || storedID == "" {
			return "", wakeErr, publishErr
		}
		return parent, wakeErr, nil
	}
}

func healthEscalationBody(origin string, report *HealthReport) string {
	lines := []string{
		fmt.Sprintf("%s healthcheck is %s.", origin, report.State()),
	}
	if report.Reason != "" {
		lines = append(lines, "", report.Reason)
	}
	if !report.LastCheckedAt.IsZero() {
		lines = append(lines, "", "last_checked_at: "+report.LastCheckedAt.UTC().Format(time.RFC3339))
	}
	if !report.LastActivityAt.IsZero() {
		lines = append(lines, "last_activity_at: "+report.LastActivityAt.UTC().Format(time.RFC3339))
	}
	return strings.Join(lines, "\n")
}

func persistHealthNotification(store *state.Store, name string, notifyCount int) {
	now := time.Now()
	if err := store.Update(name, func(s *domain.Session) error {
		if s.Health == nil {
			s.Health = &contract.HealthState{}
		}
		s.Health.LastNotifiedAt = now
		s.Health.NotifyCount = notifyCount
		s.UpdatedAt = now
		return nil
	}); err != nil {
		slog.Warn("persist health notification failed", "session", name, "error", err)
	}
}

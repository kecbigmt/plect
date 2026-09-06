package service

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func checkActionForResult(sessionName, instance, resource string, dw *config.DoneWhen, st *contract.TaskState, result task.DoneWhenResult, trigger TickTrigger) CheckAction {
	heartbeatBudget := doneWhenHeartbeatBudget(dw)
	heartbeatTicks := 0
	heartbeatEscalations := 0
	if st.DoneWhen != nil {
		heartbeatTicks = st.DoneWhen.HeartbeatTicks
		heartbeatEscalations = st.DoneWhen.HeartbeatEscalations
	}
	fingerprint := checkFingerprint(result)

	if result.Overall == task.DoneSatisfied {
		return CheckAction{
			SessionName:      sessionName,
			Instance:         instance,
			Action:           "satisfied",
			HeartbeatBudget:  heartbeatBudget,
			Summary:          fmt.Sprintf("done_when satisfied for %s", instance),
			Fingerprint:      fingerprint,
			HeartbeatChanged: true,
		}
	}

	unmetItems := unsatisfiedLeafItems(result)
	items := unmetItemSummaries(unmetItems)
	if result.Overall == task.DonePending {
		unmetItems = pendingJudgeItems(result)
		items = unmetItemSummaries(unmetItems)
	}

	nextHeartbeatTicks := heartbeatTicks
	consumedHeartbeat := false
	if trigger == TickTriggerHeartbeat {
		nextHeartbeatTicks++
		consumedHeartbeat = true
	}
	if len(unmetItems) == 0 && result.Overall == task.DonePending {
		return CheckAction{
			SessionName:      sessionName,
			Instance:         instance,
			Action:           "wait",
			HeartbeatTicks:   nextHeartbeatTicks,
			HeartbeatBudget:  heartbeatBudget,
			Fingerprint:      fingerprint,
			HeartbeatChanged: consumedHeartbeat,
		}
	}

	if heartbeatBudget > 0 && consumedHeartbeat && nextHeartbeatTicks >= heartbeatBudget {
		nextEscalation := heartbeatEscalations + 1
		body := fmt.Sprintf("done_when remains unmet for %s after %d/%d heartbeat tick(s).\n\nUnmet items:\n%s", instance, nextHeartbeatTicks, heartbeatBudget, unmetItemBulletList(unmetItems))
		return CheckAction{
			SessionName:         sessionName,
			Instance:            instance,
			Action:              "escalate",
			HeartbeatTicks:      nextHeartbeatTicks,
			HeartbeatBudget:     heartbeatBudget,
			HeartbeatEscalation: nextEscalation,
			HeartbeatChanged:    true,
			Items:               items,
			UnmetItems:          unmetItems,
			Summary:             fmt.Sprintf("done_when non-convergence for %s", instance),
			Body:                body,
			Fingerprint:         fingerprint,
			EscalationKind:      escalationKindDoneWhenNonConvergence,
		}
	}

	if result.Overall == task.DonePending {
		cmd := reviewerDispatchCommand(resource, instance)
		judgeCmds := judgeCommands(sessionName, instance, unmetItems)
		var warnings []string
		if cmd == "" {
			warnings = append(warnings, "reviewer dispatch command unavailable: task instance has no resource")
			items = append(items, warnings...)
		}
		body := reviewRequiredBody(instance, nextHeartbeatTicks, heartbeatBudget, cmd, unmetItems, judgeCmds, warnings)
		return CheckAction{
			SessionName:      sessionName,
			Instance:         instance,
			Action:           "review_required",
			HeartbeatTicks:   nextHeartbeatTicks,
			HeartbeatBudget:  heartbeatBudget,
			HeartbeatChanged: consumedHeartbeat,
			Items:            items,
			UnmetItems:       unmetItems,
			Warnings:         warnings,
			Summary:          fmt.Sprintf("done_when review required for %s", instance),
			Body:             body,
			ReviewerCommand:  cmd,
			JudgeCommands:    judgeCmds,
			Fingerprint:      fingerprint,
		}
	}
	body := fmt.Sprintf("done_when is unsatisfied for %s (heartbeat budget %s).\n\nAddress these unmet items:\n%s", instance, heartbeatBudgetText(nextHeartbeatTicks, heartbeatBudget), unmetItemBulletList(unmetItems))
	if hint := mergeableStateHint(observedState(st)); hint != "" {
		body += "\n\n" + hint
	}
	return CheckAction{
		SessionName:      sessionName,
		Instance:         instance,
		Action:           "kick",
		HeartbeatTicks:   nextHeartbeatTicks,
		HeartbeatBudget:  heartbeatBudget,
		HeartbeatChanged: consumedHeartbeat,
		Items:            items,
		UnmetItems:       unmetItems,
		Summary:          fmt.Sprintf("done_when unsatisfied for %s", instance),
		Body:             body,
		Fingerprint:      fingerprint,
	}
}

func reviewerDispatchCommand(resource, instance string) string {
	if resource == "" {
		return ""
	}
	// Which reviewer workflow runs is a chaining concern, not a judge-leaf field;
	// this advisory suggestion has a fixed interim default until chaining
	// (slice 6) owns it.
	return fmt.Sprintf("plect up %q --workflow claude --task review --tag %s", resource, reviewerTag(instance)) // boundary-allow: This hardcodes a fixed interim default for an advisory suggestion, and chaining (slice 6) will remove it once it picks the reviewer workflow instead.
}

func judgeCommands(sessionName, instance string, items []CheckUnmetItem) []string {
	var out []string
	for _, item := range items {
		if item.Kind != "judge" || item.ID == "" {
			continue
		}
		out = append(out,
			judgeCommand("approve", sessionName, instance, item.ID),
			judgeCommand("request-changes", sessionName, instance, item.ID),
		)
	}
	return out
}

func judgeCommand(action, sessionName, instance, id string) string {
	return fmt.Sprintf("plect judge %s %q %q %q --reason %q", action, sessionName, instance, id, "<reason>")
}

func reviewerTag(instance string) string {
	var b strings.Builder
	b.WriteString("review-")
	for _, r := range instance {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

const defaultHeartbeatBudget = 3

func doneWhenHeartbeatBudget(dw *config.DoneWhen) int {
	if dw == nil {
		return 0
	}
	if len(dw.Budget) == 0 {
		return defaultHeartbeatBudget
	}
	v, ok := dw.Budget["heartbeat_budget"]
	if !ok {
		return defaultHeartbeatBudget
	}
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	default:
		return 0
	}
}

func checkFingerprint(result task.DoneWhenResult) string {
	parts := make([]string, 0, len(result.Leaves)+1)
	parts = append(parts, string(result.Overall))
	for _, leaf := range result.Leaves {
		parts = append(parts, strings.Join([]string{
			leaf.Kind,
			leaf.ID,
			leaf.Output,
			string(leaf.Status),
			leaf.Value,
			leaf.Action,
			leaf.Revision,
			leaf.CurrentRevision,
			leaf.JudgeSession,
			leaf.JudgeWorkflow,
			leaf.Relation,
			leaf.Reason,
			leaf.PendingReason,
		}, "\x00"))
	}
	return strings.Join(parts, "\x01")
}

func unsatisfiedLeafItems(result task.DoneWhenResult) []CheckUnmetItem {
	var out []CheckUnmetItem
	for _, leaf := range result.Leaves {
		if leaf.Status != task.DoneUnsatisfied {
			continue
		}
		out = append(out, checkUnmetItem(leaf))
	}
	slices.SortFunc(out, compareCheckUnmetItem)
	return out
}

func pendingJudgeItems(result task.DoneWhenResult) []CheckUnmetItem {
	var out []CheckUnmetItem
	for _, leaf := range result.Leaves {
		if leaf.Kind != "judge" || leaf.Status != task.DonePending {
			continue
		}
		out = append(out, checkUnmetItem(leaf))
	}
	slices.SortFunc(out, compareCheckUnmetItem)
	return out
}

func checkUnmetItem(leaf task.DoneLeafResult) CheckUnmetItem {
	return CheckUnmetItem{
		Kind:            leaf.Kind,
		Expr:            leaf.Expr,
		Status:          leaf.Status,
		ID:              leaf.ID,
		Output:          leaf.Output,
		Value:           leaf.Value,
		Observed:        leaf.Observed,
		Action:          leaf.Action,
		Reason:          leaf.Reason,
		PendingReason:   leaf.PendingReason,
		Revision:        leaf.Revision,
		CurrentRevision: leaf.CurrentRevision,
		JudgeSession:    leaf.JudgeSession,
		JudgeWorkflow:   leaf.JudgeWorkflow,
		Relation:        leaf.Relation,
	}
}

func compareCheckUnmetItem(a, b CheckUnmetItem) int {
	return strings.Compare(unmetItemSummary(a), unmetItemSummary(b))
}

func unmetItemSummaries(items []CheckUnmetItem) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = unmetItemSummary(item)
	}
	return out
}

func unmetItemSummary(item CheckUnmetItem) string {
	summary := item.Expr
	if item.Kind == "check" {
		if item.Observed {
			summary = fmt.Sprintf("%s (observed %s)", summary, item.Value)
		} else {
			summary = fmt.Sprintf("%s (unobserved)", summary)
		}
	}
	if item.Kind == "judge" {
		if item.ID != "" {
			summary = fmt.Sprintf("%s (%s)", summary, item.ID)
		}
		switch {
		case item.PendingReason != "":
			summary = fmt.Sprintf("%s: %s", summary, item.PendingReason)
		case item.Reason != "":
			summary = fmt.Sprintf("%s: %s", summary, item.Reason)
		}
	}
	return summary
}

// observedState is what the instance's resource last reported, which is
// where an advisory reads a fact the completion predicate does not gate on.
func observedState(st *contract.TaskState) map[string]any {
	if st == nil || st.Observed == nil {
		return nil
	}
	return st.Observed.State
}

// mergeableStateHint surfaces a PR merge conflict as an advisory kick note
// rather than a done_when leaf: a review host generally does not run CI on a
// change with merge conflicts, so checks_status would otherwise sit PENDING
// forever and the session would wait indefinitely for CI that will never
// arrive. "unknown" (mergeability still being computed) and "NULL" (nothing
// to read it from) are intentionally silent.
func mergeableStateHint(outputs map[string]any) string {
	v, ok := outputs[outputKeyMergeableState]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	if s != "dirty" {
		return ""
	}
	return "Note: mergeable_state=dirty — this PR conflicts with its base branch. Rebase or merge the base branch in before continuing."
}

func unmetItemBulletList(items []CheckUnmetItem) string {
	if len(items) == 0 {
		return "- (none)"
	}
	lines := make([]string, len(items))
	for i, item := range items {
		lines[i] = "- " + unmetItemSummary(item)
		if item.Kind == "judge" {
			var details []string
			if item.Action != "" {
				details = append(details, "action="+item.Action)
			}
			if item.JudgeSession != "" {
				details = append(details, "judge="+item.JudgeSession)
			}
			if item.JudgeWorkflow != "" {
				details = append(details, "judge_workflow="+item.JudgeWorkflow)
			}
			if item.Relation != "" {
				details = append(details, "relation="+item.Relation)
			}
			if item.Revision != "" {
				details = append(details, "judge_revision="+item.Revision)
			}
			if item.CurrentRevision != "" {
				details = append(details, "current_revision="+item.CurrentRevision)
			}
			if len(details) > 0 {
				lines[i] += "\n  " + strings.Join(details, " ")
			}
		}
	}
	return strings.Join(lines, "\n")
}

func reviewRequiredBody(instance string, heartbeatTicks, heartbeatBudget int, reviewerCommand string, items []CheckUnmetItem, judgeCmds []string, warnings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "done_when needs independent review for %s (heartbeat budget %s).\n\n", instance, heartbeatBudgetText(heartbeatTicks, heartbeatBudget))
	if reviewerCommand != "" {
		fmt.Fprintf(&b, "Dispatch reviewer:\n%s\n\n", reviewerCommand)
	} else {
		b.WriteString("Dispatch reviewer:\n")
		for _, warning := range warnings {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
		b.WriteString("\n")
	}
	b.WriteString("Review these judge leaves:\n")
	b.WriteString(unmetItemBulletList(items))
	if len(judgeCmds) > 0 {
		b.WriteString("\n\nReviewer must record one action per judge leaf:\n")
		for _, cmd := range judgeCmds {
			fmt.Fprintf(&b, "- %s\n", cmd)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func heartbeatBudgetText(heartbeatTicks, heartbeatBudget int) string {
	if heartbeatBudget == 0 {
		return fmt.Sprintf("%d/unbounded", heartbeatTicks)
	}
	return fmt.Sprintf("%d/%d", heartbeatTicks, heartbeatBudget)
}

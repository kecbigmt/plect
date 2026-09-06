package lang

import "testing"

// TestSurfaceRootsMatchValuesTable walks the per-surface root table in
// docs/language/values.md: one accepted and one rejected projection per
// surface, so a row cannot be dropped or widened without a failure here.
func TestSurfaceRootsMatchValuesTable(t *testing.T) {
	tests := []struct {
		surface *Surface
		offered []string
		foreign []string
	}{
		{surfaceProviderName, []string{"match.owner", "match.repo"}, []string{"session.name", "inputs.owner"}},
		{surfaceProviderSetup, []string{"resource.id", "session.name", "session.inputs.owner", "inputs.owner", "prev.dir", "config.workspace_dirs_root"}, []string{"event.type", "self.outputs.dir", "force"}},
		{surfaceProviderCleanup, []string{"self.outputs.workspace_dir", "inputs.x", "cleanup.inputs.delete_branch", "session.name", "config.workspace_dirs_root", "force"}, []string{"resource.id", "prev.dir"}},
		{surfaceProviderSubscribe, []string{"session.name", "session.branch", "resource.id"}, []string{"inputs.x", "workspace.dir"}},
		{surfaceProviderUnsubscribe, []string{"session.name", "resource.id"}, []string{"inputs.x", "workspace.dir"}},
		{surfaceObserverObserve, []string{"resource.id", "workspace.dir", "workspace.branch"}, []string{"resource.revision", "judges", "session.name"}},
		{surfaceObserverFinalize, []string{"resource.id", "session.name", "resource.revision", "judges"}, []string{"workspace.dir", "effect.instance", "inputs.x"}},
		{surfaceWorkflowDisplay, []string{"workflow.outputs.concept_id", "session.inputs.task"}, []string{"nodes.pane.outputs.session_name", "resource.id"}},
		{surfaceWorkflowNodeInputs, []string{"nodes.pane.outputs.session_name", "workflow.outputs.x", "session.name", "session.inputs.task", "workspace.dir"}, []string{"resource.id", "locals.x", "event.type"}},
		{surfaceChannelDelivery, []string{"event.type", "event.body", "event.metadata.url", "inputs.queue_dir"}, []string{"workflow.outputs.branch", "session.name"}},
		{surfaceChannelTimeout, []string{"inputs.enqueue_timeout"}, []string{"event.metadata.timeout", "session.name"}},
		{surfaceEffectSetup, []string{"inputs.owner", "prev.x", "nodes.pane.outputs.session_name", "workflow.outputs.x", "session.name", "session.inputs.owner", "workspace.dir", "resource.id"}, []string{"event.metadata.owner", "self.outputs.x", "locals.x", "resource.state.checks_status"}},
		{surfaceEffectCleanup, []string{"self.outputs.session_name", "inputs.x", "nodes.pane.outputs.x", "workflow.outputs.x", "session.name", "workspace.dir"}, []string{"resource.id", "prev.x"}},
		{surfaceEffectHealth, []string{"self.outputs.session_name", "inputs.x", "session.name", "workspace.dir"}, []string{"nodes.pane.outputs.x", "resource.id"}},
		{surfaceEffectTerminal, []string{"self.outputs.session_name", "session.name"}, []string{"inputs.x", "workspace.dir"}},
		{surfaceEffectInner, []string{"inputs.tmux_session", "locals.guard_dir", "nodes.pane.outputs.session_name", "workflow.outputs.x", "session.name", "workspace.dir"}, []string{"inner.outputs.pid", "resource.id"}},
		{surfaceEffectOutputsBind, []string{"inner.outputs.pid", "locals.guard_dir", "inputs.mcp_servers"}, []string{"session.inputs.owner", "resource.state.checks_status", "inner.inputs.x"}},
		{surfaceTaskCompletion, []string{"resource.state.resource_kind", "self.state.verdict_revision"}, []string{"resource.id", "nodes.pane.outputs.session_name", "inputs.x"}},
		{surfaceTaskInstruction, []string{"resource.id", "resource.state.revision", "self.state.verdict_revision", "inputs.instruction", "session.name", "workflow.outputs.x"}, []string{"nodes.pane.outputs.session_name", "locals.x"}},
		{surfaceChainInputs, []string{"task.session", "task.instance", "task.workflow", "task.done_when.pending_judge_ids", "resource.state.checklist_status", "self.state.x"}, []string{"locals.guard_dir", "inputs.x"}},
	}
	for _, tc := range tests {
		t.Run(tc.surface.Name, func(t *testing.T) {
			for _, path := range tc.offered {
				if !tc.surface.offers(path) {
					t.Errorf("%s does not offer %q, but values.md lists it", tc.surface.Name, path)
				}
			}
			for _, path := range tc.foreign {
				if tc.surface.offers(path) {
					t.Errorf("%s offers %q, which values.md does not list", tc.surface.Name, path)
				}
			}
		})
	}
}

func TestSurfaceRootsAcceptAWholeRoot(t *testing.T) {
	if !surfaceChannelDelivery.offers("event") {
		t.Error("event.* covers the whole event, which a channel body serializes")
	}
	if !surfaceObserverFinalize.offers("judges") {
		t.Error("judges is a whole root on the finalize surface")
	}
	if surfaceObserverObserve.offers("workspace") {
		t.Error("the observe surface lists workspace.dir and workspace.branch, not workspace itself")
	}
}

func TestSurfaceRootsAcceptANestedContractField(t *testing.T) {
	// Whether a contract declares the field is PLECTURE-CFG-FROM-PATH's
	// question, not this one's.
	if !surfaceEffectSetup.offers("inputs.mcp_servers.name") {
		t.Error("inputs.<key> covers a field nested inside the input contract")
	}
}

func TestSurfaceIdentifiers(t *testing.T) {
	got := surfaceTaskCompletion.identifiers()
	want := []string{"resource", "self"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

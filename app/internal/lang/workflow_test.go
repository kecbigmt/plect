package lang

import "testing"

func workflowDef(t *testing.T, body string) *Definition {
	t.Helper()
	defs, err := ParseDefinitionDocument("workflows/session.toml", []byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("parsed %d definitions, want 1", len(defs))
	}
	return defs[0]
}

func TestWorkflowNodes_DerivesIDsAndReads(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[[session.nodes]]
uses = "pane"

[[session.nodes]]
id   = "worker"
uses = "official.codex.exec_runtime"

[session.nodes.inputs]
tmux_session = { from = "nodes.pane.outputs.session_name" }
label        = { expr = "'run-' + nodes.pane.outputs.session_name + session.name" }
literal      = "fixed"
`)
	nodes, err := WorkflowNodes(def)
	if err != nil {
		t.Fatalf("WorkflowNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
	if nodes[0].ID != "pane" || nodes[0].Uses != "pane" {
		t.Fatalf("node 0 = %+v, want id/uses pane", nodes[0])
	}
	if nodes[1].ID != "worker" || nodes[1].Uses != "official.codex.exec_runtime" {
		t.Fatalf("node 1 = %+v", nodes[1])
	}
	if len(nodes[1].Reads) != 1 || nodes[1].Reads[0] != "pane" {
		t.Fatalf("node 1 reads = %v, want [pane]", nodes[1].Reads)
	}
}

func TestWorkflowNodes_QualifiedUsesDefaultsToReferencedID(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[[session.nodes]]
uses = "official.tmux.pane"
`)
	nodes, err := WorkflowNodes(def)
	if err != nil {
		t.Fatalf("WorkflowNodes: %v", err)
	}
	if nodes[0].ID != "pane" {
		t.Fatalf("defaulted id = %q, want pane", nodes[0].ID)
	}
}

func TestWorkflowNodes_DuplicateIDRejected(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[[session.nodes]]
uses = "pane"

[[session.nodes]]
id   = "pane"
uses = "official.tmux.other"
`)
	_, err := WorkflowNodes(def)
	assertDiagnostic(t, err, CodeIDDuplicate, LayerSemantic)
}

func TestWorkflowNodes_NodeWithoutUsesRejected(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[[session.nodes]]
id = "worker"
`)
	_, err := WorkflowNodes(def)
	assertDiagnostic(t, err, CodeFieldRequired, LayerStructural)
}

func TestWorkflowNodes_UnknownNodeFieldRejected(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[[session.nodes]]
uses       = "pane"
depends_on = ["other"]
`)
	_, err := WorkflowNodes(def)
	assertDiagnostic(t, err, CodeFieldUnknown, LayerStructural)
}

// A misspelled clock field is a declaration with no consumer: read as unset,
// it would leave the author believing the cadence is in force.
func TestValidateWorkflow_UnknownClockFieldRejected(t *testing.T) {
	for _, body := range []string{
		"[session.tick]\nstale_when = \"15m\"\n",
		"[session.healthcheck]\nperiod_seconds = 300\n",
	} {
		def := workflowDef(t, "[session]\nkind = \"workflow\"\n\n"+body)
		assertDiagnostic(t, Validation{}.ValidateDefinition(def), CodeFieldUnknown, LayerStructural)
	}
}

func TestValidateWorkflow_CycleThroughBlocksRejected(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[[session.nodes]]
uses   = "pane"
blocks = ["reaper"]

[session.nodes.inputs]
peer = { from = "nodes.reaper.outputs.pid" }

[[session.nodes]]
uses = "reaper"
`)
	err := Validation{}.ValidateDefinition(def)
	assertDiagnostic(t, err, CodeWorkflowCycle, LayerSemantic)
}

// A cascade layer may add a node whose inputs read a base layer's node, so an
// edge to an id this definition does not declare is the merged load's
// question, not this pass's.
func TestValidateWorkflow_EdgeToUndeclaredNodeIsNotACycle(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[[session.nodes]]
uses = "reaper"

[session.nodes.inputs]
peer = { from = "nodes.pane.outputs.session_name" }
`)
	if err := (Validation{}).ValidateDefinition(def); err != nil {
		t.Fatalf("ValidateDefinition: %v", err)
	}
}

// A value surface accepts a whole JSON document built from projections, so an
// edge can be declared arbitrarily deep inside one. Validation already walks
// those leaves; the topology has to walk the same ones or the graph it reports
// is not the graph the wiring declares.
func TestWorkflowNodes_ReadsThroughAJSONOperand(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[[session.nodes]]
uses = "producer"

[[session.nodes]]
uses = "consumer"

[session.nodes.inputs]
payload = { json = { nested = { list = [ { from = "nodes.producer.outputs.value" } ] } } }
`)
	nodes, err := WorkflowNodes(def)
	if err != nil {
		t.Fatalf("WorkflowNodes: %v", err)
	}
	if len(nodes[1].Reads) != 1 || nodes[1].Reads[0] != "producer" {
		t.Fatalf("consumer reads = %v, want [producer]", nodes[1].Reads)
	}
}

// A provider repair's invalidation pass asks whether a node reads a
// provider output at all, not which sibling node it reads, so ProjectedRoots
// must surface the raw workflow.outputs / workspace paths NodeReads' node-id
// filter discards.
func TestProjectedRoots_SurfacesNonNodePaths(t *testing.T) {
	inputs := map[string]*Value{
		"wd":     {Form: FormFrom, From: "workflow.outputs.workspace_dir"},
		"branch": {Form: FormFrom, From: "workspace.branch"},
		"peer":   {Form: FormFrom, From: "nodes.producer.outputs.value"},
	}
	got := ProjectedRoots(inputs)
	want := map[string]bool{
		"workflow.outputs.workspace_dir": true,
		"workspace.branch":               true,
		"nodes.producer.outputs.value":   true,
	}
	if len(got) != len(want) {
		t.Fatalf("ProjectedRoots = %v, want %v", got, want)
	}
	for _, path := range got {
		if !want[path] {
			t.Errorf("ProjectedRoots returned unexpected path %q", path)
		}
	}
}

func TestValidateWorkflow_CycleThroughAJSONOperandRejected(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[[session.nodes]]
uses = "first"

[session.nodes.inputs]
payload = { json = { peer = { from = "nodes.second.outputs.value" } } }

[[session.nodes]]
uses = "second"

[session.nodes.inputs]
peer = { from = "nodes.first.outputs.value" }
`)
	assertDiagnostic(t, Validation{}.ValidateDefinition(def), CodeWorkflowCycle, LayerSemantic)
}

// A channel binding that is not an array of tables reaches the runtime as no
// channels at all, so the session would sit unreachable with nothing said.
func TestValidateWorkflow_NonArrayEventChannelRejected(t *testing.T) {
	def := workflowDef(t, `
[session]
kind = "workflow"

[session.event]
channel = "runtime"

[[session.nodes]]
uses = "pane"
`)
	assertDiagnostic(t, Validation{}.ValidateDefinition(def), CodeFieldType, LayerStructural)
}

func TestValidateWorkflow_EventChannelRequiresNameAndUses(t *testing.T) {
	for _, body := range []string{
		"[[session.event.channel]]\nuses = \"delivery\"\n",
		"[[session.event.channel]]\nname = \"runtime\"\n",
	} {
		def := workflowDef(t, "[session]\nkind = \"workflow\"\n\n[[session.nodes]]\nuses = \"pane\"\n\n"+body)
		assertDiagnostic(t, Validation{}.ValidateDefinition(def), CodeFieldRequired, LayerStructural)
	}
}

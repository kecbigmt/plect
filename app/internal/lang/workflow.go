package lang

import (
	"fmt"
	"strings"
)

// WorkflowNode is one entry of a workflow's `[[nodes]]` array, read for the
// topology its wiring declares rather than for the effect it selects.
//
// Reads holds the node ids this node's input values project from, in
// first-appearance order, whether or not this definition declares them: a
// cascade layer may add a node whose inputs read a base layer's node, so
// which ids resolve is a question only the merged definition answers.
type WorkflowNode struct {
	ID     string
	Uses   string
	Reads  []string
	Blocks []string
}

// nodeReadPrefix is the projection that declares a dependency edge:
// workflows.md derives the setup and cleanup graph from the node bindings
// rather than from a `depends_on` field, so this prefix is the whole edge
// vocabulary.
const nodeReadPrefix = "nodes."

// WorkflowNodes reads one workflow definition's node array as the topology it
// declares: each node's id (defaulted from the referenced definition's id),
// the node ids its input values read, and whether those edges form a cycle.
//
// Reading the topology and rejecting a cyclic one are one call because a
// caller that derives execution order from this needs the same answer the
// load-time check gave.
func WorkflowNodes(def *Definition) ([]WorkflowNode, error) {
	pos := Position{File: def.File, Path: def.ID}
	raw, err := tableArray(def.Body, "nodes", pos)
	if err != nil {
		return nil, err
	}
	out := make([]WorkflowNode, 0, len(raw))
	declared := make(map[string]int, len(raw))
	for i, entry := range raw {
		at := childPos(childPos(pos, "nodes"), fmt.Sprintf("[%d]", i))
		node, err := workflowNode(entry, at)
		if err != nil {
			return nil, err
		}
		if _, dup := declared[node.ID]; dup {
			return nil, newDiag(CodeIDDuplicate, LayerSemantic, at,
				fmt.Sprintf("node id %q is declared more than once in workflow %q", node.ID, def.ID))
		}
		declared[node.ID] = i
		out = append(out, node)
	}
	if err := checkWorkflowGraph(out, pos); err != nil {
		return nil, err
	}
	return out, nil
}

// WorkflowNodeTables and WorkflowEventChannels hand a decoder the same
// array-of-tables the validator read, so a malformed one is the language's
// diagnostic rather than a nil slice the decoder would read as "declares
// none".
func WorkflowNodeTables(def *Definition) ([]map[string]any, error) {
	return tableArray(def.Body, "nodes", Position{File: def.File, Path: def.ID})
}

func WorkflowEventChannels(def *Definition) ([]map[string]any, error) {
	return Validation{}.eventChannels(def, Position{File: def.File, Path: def.ID})
}

func workflowNode(entry map[string]any, pos Position) (WorkflowNode, error) {
	var node WorkflowNode
	if err := rejectUnknownFields(entry, pos, "id", "uses", "inputs", "blocks"); err != nil {
		return node, err
	}
	raw, declared := entry["uses"]
	if !declared {
		return node, newDiag(CodeFieldRequired, LayerStructural, pos, "a node selects its effect through `uses`")
	}
	uses, err := staticRef(raw, childPos(pos, "uses").Path)
	if err != nil {
		return node, err
	}
	node.Uses = uses
	if raw, ok := entry["id"]; ok {
		id, isString := raw.(string)
		if !isString {
			return node, newDiag(CodeFieldType, LayerStructural, childPos(pos, "id"), "a node id is a string")
		}
		node.ID = id
	} else {
		// The referenced definition's id, which for a catalog-qualified
		// reference is its last segment: the ownership path in front of it
		// addresses the layer, not the definition.
		node.ID = uses[strings.LastIndex(uses, ".")+1:]
	}
	if !isValidID(node.ID) {
		return node, newDiag(CodeIDInvalid, LayerStructural, childPos(pos, "id"),
			fmt.Sprintf("node id %q does not match ^[A-Za-z_][A-Za-z0-9_]*$", node.ID))
	}
	inputs, err := nodeInputValues(entry, pos)
	if err != nil {
		return node, err
	}
	node.Reads = NodeReads(inputs)
	blocks, err := nodeBlocks(entry, pos)
	if err != nil {
		return node, err
	}
	node.Blocks = blocks
	return node, nil
}

func nodeInputValues(entry map[string]any, pos Position) (map[string]*Value, error) {
	raw, ok := entry["inputs"]
	if !ok {
		return nil, nil
	}
	at := childPos(pos, "inputs")
	tbl, err := table(raw, at)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*Value, len(tbl))
	for _, key := range sortedKeys(tbl) {
		value, err := ParseValue(tbl[key], ClassData, childPos(at, key))
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

func nodeBlocks(entry map[string]any, pos Position) ([]string, error) {
	raw, ok := entry["blocks"]
	if !ok {
		return nil, nil
	}
	at := childPos(pos, "blocks")
	list, ok := raw.([]any)
	if !ok {
		return nil, newDiag(CodeFieldType, LayerStructural, at, "`blocks` is a list of node ids")
	}
	out := make([]string, 0, len(list))
	for _, entry := range list {
		id, ok := entry.(string)
		if !ok {
			return nil, newDiag(CodeFieldType, LayerStructural, at, "`blocks` is a list of node ids")
		}
		out = append(out, id)
	}
	return out, nil
}

// NodeReads collects the node ids one node's input wiring projects from, in
// key order so a diagnostic naming the first offender is reproducible. It is
// the whole edge vocabulary of the workflow dependency graph: the load-time
// cycle check and the runtime's execution order both derive their edges here,
// so they cannot disagree about what a wiring declares.
func NodeReads(inputs map[string]*Value) []string {
	var out []string
	seen := map[string]bool{}
	for _, path := range ProjectedRoots(inputs) {
		id, ok := nodeReadID(path)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ProjectedRoots collects every root path one node's input wiring projects
// from, in key order. It is NodeReads' traversal without the node-id filter,
// for a consumer that needs the raw paths themselves — a provider repair's
// invalidation pass, which asks whether a node reads `workflow.outputs.*` or
// `workspace.*` rather than which sibling node it reads.
func ProjectedRoots(inputs map[string]*Value) []string {
	var out []string
	seen := map[string]bool{}
	add := func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	for _, key := range sortedValueKeys(inputs) {
		valueReads(inputs[key], add)
	}
	return out
}

// valueReads walks one value for the projections it makes, descending into a
// JSON operand's own leaves: a value surface accepts a whole document built
// from projections, so an edge can be declared arbitrarily deep inside one
// and a walk that stopped at the top would miss it.
func valueReads(value *Value, add func(string)) {
	if value == nil {
		return
	}
	switch value.Form {
	case FormFrom:
		add(value.From)
	case FormExpr:
		for _, path := range expressionPaths(value.Expr, surfaceWorkflowNodeInputs) {
			add(path)
		}
	case FormJSON:
		operandReads(value.JSON, add)
	}
}

func operandReads(op *JSONOperand, add func(string)) {
	switch {
	case op == nil:
		return
	case op.Leaf != nil:
		valueReads(op.Leaf, add)
	case op.Object != nil:
		for _, key := range sortedOperandKeys(op.Object) {
			operandReads(op.Object[key], add)
		}
	default:
		for _, child := range op.Array {
			operandReads(child, add)
		}
	}
}

// nodeReadID reads the node id out of a `nodes.<id>.outputs.<key>`
// projection. A shorter path names no output, so it declares no edge.
func nodeReadID(path string) (string, bool) {
	if !strings.HasPrefix(path, nodeReadPrefix) {
		return "", false
	}
	segments := strings.Split(path, ".")
	if len(segments) < 3 {
		return "", false
	}
	return segments[1], true
}

func sortedValueKeys(values map[string]*Value) []string {
	keys := make(map[string]any, len(values))
	for key := range values {
		keys[key] = nil
	}
	return sortedKeys(keys)
}

// checkWorkflowGraph rejects a cycle in the dependency graph the node wiring
// derives. An edge to a node this definition does not declare is left to the
// merged load, which is the only place the answer exists.
func checkWorkflowGraph(nodes []WorkflowNode, pos Position) error {
	edges := make(map[string][]string, len(nodes))
	declared := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		declared[node.ID] = true
	}
	for _, node := range nodes {
		for _, read := range node.Reads {
			if declared[read] {
				edges[node.ID] = append(edges[node.ID], read)
			}
		}
		for _, blocked := range node.Blocks {
			if declared[blocked] {
				edges[blocked] = append(edges[blocked], node.ID)
			}
		}
	}
	state := make(map[string]int, len(nodes))
	var walk func(id string) []string
	walk = func(id string) []string {
		switch state[id] {
		case 1:
			return []string{id}
		case 2:
			return nil
		}
		state[id] = 1
		for _, dep := range edges[id] {
			if cycle := walk(dep); cycle != nil {
				return append(cycle, id)
			}
		}
		state[id] = 2
		return nil
	}
	for _, node := range nodes {
		if cycle := walk(node.ID); cycle != nil {
			return newDiag(CodeWorkflowCycle, LayerSemantic, childPos(pos, "nodes"),
				fmt.Sprintf("the dependencies derived from the node wiring form a cycle: %s", strings.Join(reversed(cycle), " -> ")))
		}
	}
	return nil
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

package lang

import "fmt"

// kindFields is the per-kind surface the language enforces, and the authority
// for it: the conformance corpus asserts the diagnostics this produces, while
// plecture.schema.json describes the same shape for editors. `kind` is absent
// because parseDefinitionTable lifts it out of Body. Nothing derives one from
// the other at run time — the schema is a repository file, not a package
// resource — so TestKindSurfaceMatchesSchema is what keeps the two together.
var kindFields = map[Kind]map[string]bool{
	KindEffect: fieldSet(
		"cleanup", "health", "inner", "inputs_schema", "inputs_schema_file",
		"locals_schema", "locals_schema_file", "outputs", "outputs_schema",
		"outputs_schema_file", "scope", "setup", "terminal"),
	KindTask: fieldSet(
		"budget", "chains", "description", "done_when", "extends", "inputs_schema",
		"inputs_schema_file", "instructions",
		"resource_observer", "state_schema", "state_schema_file"),
	KindChannel: fieldSet(
		"args", "bin", "bind", "body", "command", "input_schema", "path",
		"script", "stdin", "timeout", "type"),
	KindWorkflow: fieldSet(
		"auto_select", "description", "display", "event", "healthcheck",
		"inputs_schema", "inputs_schema_file", "max_up_children", "name",
		"nodes", "populations", "tick", "workspace_provider", "workspace_provider_inputs"),
	KindWorkspaceProvider: fieldSet(
		"cleanup", "health", "inputs_schema", "inputs_schema_file", "match", "name",
		"outputs_schema", "outputs_schema_file", "setup", "subscribe",
		"unsubscribe"),
	KindResourceObserver: fieldSet(
		"finalize", "match", "observe", "query", "state_schema", "state_schema_file"),
}

func fieldSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// checkKindSurface is one rule rather than a rule per misplaced field: a
// lifecycle field on a task document and a completion contract on an effect
// are the same rule read from two directions.
func checkKindSurface(def *Definition, pos Position) error {
	fields := kindFields[def.Kind]
	for _, field := range sortedKeys(def.Body) {
		if !fields[field] {
			return newDiag(CodeFieldUnknown, LayerStructural, childPos(pos, field),
				fmt.Sprintf("%q is not part of the %s surface", field, def.Kind))
		}
	}
	return nil
}

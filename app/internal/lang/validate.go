package lang

import (
	"fmt"
	"regexp"
	"strings"
)

// Validation carries what one definition's value surfaces are checked
// against: the layer that wrote the config, and the executables a bin
// reference resolves against.
type Validation struct {
	From        Ownership
	Executables BinResolver
}

// ValidateDefinition walks exactly the surfaces docs/language/values.md
// declares for a definition's kind. Per-kind field applicability, the
// nesting joint's agreement rules, and contract-key resolution are other
// checks, and a definition that breaks one of those still passes here.
func (v Validation) ValidateDefinition(def *Definition) error {
	pos := Position{File: def.File, Path: def.ID}
	if err := checkKindSurface(def, pos); err != nil {
		return err
	}
	if err := v.checkStaticTopology(def, pos); err != nil {
		return err
	}
	for _, field := range []string{"inputs_schema", "outputs_schema", "locals_schema", "state_schema", "input_schema"} {
		if contract, ok := def.Body[field]; ok {
			if err := checkNoTaggedValues(contract, childPos(pos, field)); err != nil {
				return err
			}
		}
	}
	switch def.Kind {
	case KindEffect:
		return v.validateEffect(def, pos)
	case KindChannel:
		return v.validateChannel(def, pos)
	case KindWorkspaceProvider:
		return v.validateProvider(def, pos)
	case KindResourceObserver:
		return v.validateObserver(def, pos)
	case KindWorkflow:
		return v.validateWorkflow(def, pos)
	case KindTask:
		return v.validateTask(def, pos)
	}
	return nil
}

func (v Validation) validateEffect(def *Definition, pos Position) error {
	if err := v.action(def.Body, "setup", surfaceEffectSetup, pos); err != nil {
		return err
	}
	if err := v.action(def.Body, "cleanup", surfaceEffectCleanup, pos); err != nil {
		return err
	}
	// Each of the four tables below is closed the way the definition surface
	// itself is: a misspelled probe, verb, or joint key is a declaration with
	// no consumer, and reading it as an unset one would leave the author
	// believing it is in force.
	hasAlive := false
	if health, ok := def.Body["health"]; ok {
		at := childPos(pos, "health")
		tbl, err := table(health, at)
		if err != nil {
			return err
		}
		if err := rejectUnknownFields(tbl, at, "alive", "activity"); err != nil {
			return err
		}
		// alive is the one probe admitting the noop variant, so it parses
		// through ParseAliveAction rather than the v.action helper every
		// other action-bearing field uses.
		if raw, ok := tbl["alive"]; ok {
			hasAlive = true
			aliveAt := childPos(at, "alive")
			action, err := ParseAliveAction(raw, aliveAt)
			if err != nil {
				return err
			}
			if err := v.checkAction(action, surfaceEffectHealth, aliveAt); err != nil {
				return err
			}
		}
		if err := v.action(tbl, "activity", surfaceEffectHealth, at); err != nil {
			return err
		}
	}
	if _, hasSetup := def.Body["setup"]; hasSetup && !hasAlive {
		// A setup-bearing effect owns a surface that can vanish, so "not
		// observed" has to be a reviewed choice — a noop action — rather than
		// a declaration nobody wrote.
		return newDiag(CodeHealthAliveRequired, LayerStructural, childPos(childPos(pos, "health"), "alive"),
			"an effect with setup declares [health.alive], as an executable probe or as an explicit noop")
	}
	if terminal, ok := def.Body["terminal"]; ok {
		at := childPos(pos, "terminal")
		tbl, err := table(terminal, at)
		if err != nil {
			return err
		}
		if err := rejectUnknownFields(tbl, at, terminalVerbOrder...); err != nil {
			return err
		}
		for _, verb := range terminalVerbOrder {
			if err := v.action(tbl, verb, surfaceEffectTerminal, at); err != nil {
				return err
			}
		}
	}
	if inner, ok := def.Body["inner"]; ok {
		at := childPos(pos, "inner")
		tbl, err := table(inner, at)
		if err != nil {
			return err
		}
		if err := rejectUnknownFields(tbl, at, "uses", "inputs", "env"); err != nil {
			return err
		}
		for _, field := range []string{"inputs", "env"} {
			if err := v.valueTable(tbl, field, ClassData, surfaceEffectInner, at); err != nil {
				return err
			}
		}
	}
	if outputs, ok := def.Body["outputs"]; ok {
		at := childPos(pos, "outputs")
		tbl, err := table(outputs, at)
		if err != nil {
			return err
		}
		if err := rejectUnknownFields(tbl, at, "bind"); err != nil {
			return err
		}
		if err := v.valueTable(tbl, "bind", ClassData, surfaceEffectOutputsBind, at); err != nil {
			return err
		}
	}
	return nil
}

// ChannelAction reads the action a process-delivering channel declares. A
// channel's own `type` doubles as its action's type, and its parameters and
// deadline belong to the channel rather than to the action, so the action is
// assembled from the delivery fields alone. A unix_socket channel runs no
// process and reports a nil action.
func ChannelAction(def *Definition, pos Position) (*Action, error) {
	kind, _ := def.Body["type"].(string)
	if kind != ActionExec && kind != ActionShell {
		return nil, nil
	}
	delivery := map[string]any{"type": kind}
	for _, field := range append(execOnlyFields, shellOnlyFields...) {
		if raw, ok := def.Body[field]; ok {
			delivery[field] = raw
		}
	}
	return ParseAction(delivery, pos)
}

func (v Validation) validateChannel(def *Definition, pos Position) error {
	kind, _ := def.Body["type"].(string)
	switch kind {
	case "unix_socket":
		for _, field := range []string{"path", "body"} {
			raw, ok := def.Body[field]
			if !ok {
				return newDiag(CodeFieldRequired, LayerStructural, childPos(pos, field),
					"a unix_socket channel declares path and body")
			}
			if err := v.value(raw, ClassData, surfaceChannelDelivery, childPos(pos, field)); err != nil {
				return err
			}
		}
		for _, field := range append(execOnlyFields, shellOnlyFields...) {
			if _, ok := def.Body[field]; ok {
				return newDiag(CodeActionVariant, LayerStructural, childPos(pos, field),
					fmt.Sprintf("%s belongs to a process delivery, not to unix_socket", field))
			}
		}
	case ActionExec, ActionShell:
		for _, field := range []string{"path", "body"} {
			if _, ok := def.Body[field]; ok {
				return newDiag(CodeActionVariant, LayerStructural, childPos(pos, field),
					fmt.Sprintf("%s belongs to unix_socket delivery, not to %s", field, kind))
			}
		}
		action, err := ChannelAction(def, pos)
		if err != nil {
			return err
		}
		if err := v.checkAction(action, surfaceChannelDelivery, pos); err != nil {
			return err
		}
	default:
		return newDiag(CodeActionTypeUnknown, LayerStructural, childPos(pos, "type"),
			fmt.Sprintf("a channel's type is unix_socket, exec, or shell, not %v", def.Body["type"]))
	}
	if timeout, ok := def.Body["timeout"]; ok {
		value, err := ParseValue(timeout, ClassData, childPos(pos, "timeout"))
		if err != nil {
			return err
		}
		if value.Form != FormLiteral && value.Form != FormFrom {
			return newDiag(CodeChannelTimeoutRoot, LayerStructural, childPos(pos, "timeout"),
				"a delivery deadline is an author-declared parameter, so timeout is a literal or a projection of inputs")
		}
		if err := v.checkValue(value, surfaceChannelTimeout, childPos(pos, "timeout")); err != nil {
			return err
		}
	}
	return nil
}

func (v Validation) validateProvider(def *Definition, pos Position) error {
	if name, ok := def.Body["name"]; ok {
		if err := v.value(name, ClassData, surfaceProviderName, childPos(pos, "name")); err != nil {
			return err
		}
	}
	for _, hook := range []struct {
		field   string
		surface *Surface
	}{
		{"setup", surfaceProviderSetup},
		{"cleanup", surfaceProviderCleanup},
		{"subscribe", surfaceProviderSubscribe},
		{"unsubscribe", surfaceProviderUnsubscribe},
	} {
		if err := v.action(def.Body, hook.field, hook.surface, pos); err != nil {
			return err
		}
	}
	hasAlive := false
	if health, ok := def.Body["health"]; ok {
		at := childPos(pos, "health")
		tbl, err := table(health, at)
		if err != nil {
			return err
		}
		// A provider has no scope, so activity names no consumer here.
		if err := rejectUnknownFields(tbl, at, "alive"); err != nil {
			return err
		}
		if raw, ok := tbl["alive"]; ok {
			hasAlive = true
			aliveAt := childPos(at, "alive")
			action, err := ParseAliveAction(raw, aliveAt)
			if err != nil {
				return err
			}
			if err := v.checkAction(action, surfaceProviderHealth, aliveAt); err != nil {
				return err
			}
		}
	}
	if _, hasSetup := def.Body["setup"]; hasSetup && !hasAlive {
		return newDiag(CodeHealthAliveRequired, LayerStructural, childPos(childPos(pos, "health"), "alive"),
			"a workspace provider with setup declares [health.alive], as an executable probe or as an explicit noop")
	}
	return v.providerContracts(def, pos)
}

func (v Validation) validateObserver(def *Definition, pos Position) error {
	if err := v.action(def.Body, "observe", surfaceObserverObserve, pos); err != nil {
		return err
	}
	if err := v.action(def.Body, "finalize", surfaceObserverFinalize, pos); err != nil {
		return err
	}
	return v.validateObserverQuery(def, pos)
}

func (v Validation) validateWorkflow(def *Definition, pos Position) error {
	if err := v.valueTable(def.Body, "display", ClassData, surfaceWorkflowDisplay, pos); err != nil {
		return err
	}
	if inputs, ok := def.Body["workspace_provider_inputs"]; ok {
		at := childPos(pos, "workspace_provider_inputs")
		tbl, err := table(inputs, at)
		if err != nil {
			return err
		}
		for _, key := range sortedKeys(tbl) {
			// A workspace provider's hooks run before any node output
			// exists, so its parameters are literal data and no surface
			// environment applies.
			if _, err := ParseValue(tbl[key], ClassLiteral, childPos(at, key)); err != nil {
				return err
			}
		}
	}
	nodes, err := tableArray(def.Body, "nodes", pos)
	if err != nil {
		return err
	}
	for i, node := range nodes {
		at := childPos(childPos(pos, "nodes"), fmt.Sprintf("[%d]", i))
		if err := v.valueTable(node, "inputs", ClassData, surfaceWorkflowNodeInputs, at); err != nil {
			return err
		}
	}
	if err := v.validateWorkflowPopulations(def, pos); err != nil {
		return err
	}
	if event, ok := def.Body["event"]; ok {
		at := childPos(pos, "event")
		tbl, err := table(event, at)
		if err != nil {
			return err
		}
		if err := rejectUnknownFields(tbl, at, "channel"); err != nil {
			return err
		}
	}
	channels, err := v.eventChannels(def, pos)
	if err != nil {
		return err
	}
	for i, channel := range channels {
		at := childPos(childPos(pos, "event.channel"), fmt.Sprintf("[%d]", i))
		if err := rejectUnknownFields(channel, at, "name", "uses", "inputs", "include"); err != nil {
			return err
		}
		// A binding with no name cannot be addressed and one with no target
		// delivers nowhere, so both are required here rather than left to
		// whichever consumer notices the gap first.
		for _, field := range []string{"name", "uses"} {
			if _, declared := channel[field]; !declared {
				return newDiag(CodeFieldRequired, LayerStructural, childPos(at, field),
					fmt.Sprintf("an event channel declares `%s`", field))
			}
		}
		if err := v.valueTable(channel, "inputs", ClassData, surfaceWorkflowNodeInputs, at); err != nil {
			return err
		}
	}
	// A clock's table is closed the way the definition surface itself is: a
	// misspelled `heartbeat` or `period` is a declaration with no consumer,
	// and reading it as an unset one would leave the author believing the
	// cadence is in force while the session sits still.
	for _, clock := range []struct {
		field  string
		fields []string
	}{
		{"tick", []string{"on", "heartbeat", "max_heartbeat"}},
		{"healthcheck", []string{"period", "stall_threshold", "renotify_every"}},
	} {
		raw, ok := def.Body[clock.field]
		if !ok {
			continue
		}
		at := childPos(pos, clock.field)
		tbl, err := table(raw, at)
		if err != nil {
			return err
		}
		if err := rejectUnknownFields(tbl, at, clock.fields...); err != nil {
			return err
		}
	}
	_, err = WorkflowNodes(def)
	return err
}

func (v Validation) validateTask(def *Definition, pos Position) error {
	if _, extends := def.Body["extends"]; extends {
		if _, ok := def.Body["resource_observer"]; ok {
			return newDiag(CodeExtendsInheritedField, LayerStructural, childPos(pos, "resource_observer"),
				"resource_observer is inherited through extends and is not part of the extension surface")
		}
		if err := checkExtendsSchemaShape(def, pos); err != nil {
			return err
		}
	}
	if err := v.completionPredicate(def.Body, "done_when", pos); err != nil {
		return err
	}
	chains, err := tableArray(def.Body, "chains", pos)
	if err != nil {
		return err
	}
	for i, chain := range chains {
		at := childPos(childPos(pos, "chains"), fmt.Sprintf("[%d]", i))
		if err := v.completionPredicate(chain, "when", at); err != nil {
			return err
		}
		// The resource a fire binds its spawned session to is projected from
		// the same facts the inputs beside it are, because it is one of them:
		// what differs is that the session is bound to it rather than handed
		// it.
		if raw, declared := chain["resource"]; declared {
			if err := v.value(raw, ClassData, surfaceChainInputs, childPos(at, "resource")); err != nil {
				return err
			}
		}
		if err := v.valueTable(chain, "inputs", ClassData, surfaceChainInputs, at); err != nil {
			return err
		}
	}
	return v.instructionBody(def, pos)
}

// completionPredicate checks the leaves of a done_when or a chain's when. A
// judge leaf is skipped rather than rejected: it carries prose for a
// reviewer, not a value over this surface's roots.
func (v Validation) completionPredicate(body map[string]any, field string, pos Position) error {
	raw, ok := body[field]
	if !ok {
		return nil
	}
	tbl, err := table(raw, childPos(pos, field))
	if err != nil {
		return err
	}
	leaves, err := tableArray(tbl, "all", childPos(pos, field))
	if err != nil {
		return err
	}
	for i, leaf := range leaves {
		at := childPos(childPos(childPos(pos, field), "all"), fmt.Sprintf("[%d]", i))
		if key, ok := leaf["check"]; ok {
			path, ok := key.(string)
			if !ok {
				return newDiag(CodeFieldType, LayerStructural, at, "check names a completion key path")
			}
			if !surfaceTaskCompletion.offers(path) {
				return newDiag(CodeFromRoot, surfaceTaskCompletion.rootLayer, at,
					fmt.Sprintf("%q is not a root the %s surface offers", path, surfaceTaskCompletion.Name))
			}
		}
		if expr, ok := leaf["expr"]; ok {
			src, ok := expr.(string)
			if !ok {
				return newDiag(CodeFieldType, LayerStructural, at, "expr holds one CEL expression")
			}
			if err := checkExpression(src, surfaceTaskCompletion, at); err != nil {
				return err
			}
		}
	}
	return nil
}

// bodyProjection matches a projection in prose position. Control flow in an
// instruction body is a closed decision: a `{{ ... }}` holding anything but
// a dotted path is not a transitional template form to carry along, it is
// PLECTURE-CFG-TASK-INSTRUCTION-CONTROL-FLOW.
var bodyProjection = regexp.MustCompile(`\{\{\s*([a-z][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)*)\s*\}\}`)

func (v Validation) instructionBody(def *Definition, pos Position) error {
	if remainder := bodyProjection.ReplaceAllString(def.Instruction, ""); strings.Contains(remainder, "{{") {
		return newDiag(CodeTaskInstructionControlFlow, LayerSemantic, childPos(pos, "instruction"),
			"an instruction body permits only a {{ dotted.path }} projection, not a Go-template control-flow construct")
	}
	for _, match := range bodyProjection.FindAllStringSubmatch(def.Instruction, -1) {
		path := match[1]
		if !surfaceTaskInstruction.offers(path) {
			return newDiag(CodeFromRoot, surfaceTaskInstruction.rootLayer, childPos(pos, "instruction"),
				fmt.Sprintf("%q is not a root the %s surface offers", path, surfaceTaskInstruction.Name))
		}
	}
	return nil
}

// checkStaticTopology rejects a computed value on a field that determines
// topology, so the shape of a configuration is discoverable before anything
// is evaluated.
func (v Validation) checkStaticTopology(def *Definition, pos Position) error {
	if wp, ok := def.Body["workspace_provider"]; ok {
		if _, err := staticRef(wp, childPos(pos, "workspace_provider").Path); err != nil {
			return err
		}
	}
	if extends, ok := def.Body["extends"]; ok {
		if _, err := staticRef(extends, childPos(pos, "extends").Path); err != nil {
			return err
		}
	}
	if inner, ok := def.Body["inner"].(map[string]any); ok {
		if uses, ok := inner["uses"]; ok {
			if _, err := staticRef(uses, childPos(childPos(pos, "inner"), "uses").Path); err != nil {
				return err
			}
		}
	}
	nodes, err := tableArray(def.Body, "nodes", pos)
	if err != nil {
		return err
	}
	for i, node := range nodes {
		if uses, ok := node["uses"]; ok {
			at := childPos(childPos(childPos(pos, "nodes"), fmt.Sprintf("[%d]", i)), "uses")
			if _, err := staticRef(uses, at.Path); err != nil {
				return err
			}
		}
	}
	channels, err := v.eventChannels(def, pos)
	if err != nil {
		return err
	}
	for i, channel := range channels {
		if uses, ok := channel["uses"]; ok {
			at := childPos(childPos(childPos(pos, "event.channel"), fmt.Sprintf("[%d]", i)), "uses")
			if _, err := staticRef(uses, at.Path); err != nil {
				return err
			}
		}
	}
	chains, err := tableArray(def.Body, "chains", pos)
	if err != nil {
		return err
	}
	for i, chain := range chains {
		if workflow, ok := chain["workflow"]; ok {
			at := childPos(childPos(childPos(pos, "chains"), fmt.Sprintf("[%d]", i)), "workflow")
			if _, err := staticRef(workflow, at.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// eventChannels reads a workflow's channel bindings. A malformed `event` or
// `channel` is reported rather than read as no channels: a workflow whose
// delivery silently disappeared would be indistinguishable from one that
// declared none, and the session would sit unreachable.
func (v Validation) eventChannels(def *Definition, pos Position) ([]map[string]any, error) {
	raw, ok := def.Body["event"]
	if !ok {
		return nil, nil
	}
	at := childPos(pos, "event")
	event, err := table(raw, at)
	if err != nil {
		return nil, err
	}
	return tableArray(event, "channel", at)
}

func (v Validation) action(body map[string]any, field string, s *Surface, pos Position) error {
	raw, ok := body[field]
	if !ok {
		return nil
	}
	at := childPos(pos, field)
	action, err := ParseAction(raw, at)
	if err != nil {
		return err
	}
	return v.checkAction(action, s, at)
}

func (v Validation) checkAction(a *Action, s *Surface, pos Position) error {
	if a.Bin != "" {
		if _, err := v.Executables.ResolveBin(a.Bin, v.From); err != nil {
			return err
		}
	}
	for _, value := range a.values() {
		if err := v.checkValue(value, s, value.Pos); err != nil {
			return err
		}
	}
	return nil
}

func (v Validation) valueTable(body map[string]any, field string, class ValueClass, s *Surface, pos Position) error {
	raw, ok := body[field]
	if !ok {
		return nil
	}
	at := childPos(pos, field)
	tbl, err := table(raw, at)
	if err != nil {
		return err
	}
	for _, key := range sortedKeys(tbl) {
		if err := v.value(tbl[key], class, s, childPos(at, key)); err != nil {
			return err
		}
	}
	return nil
}

func (v Validation) value(raw any, class ValueClass, s *Surface, pos Position) error {
	value, err := ParseValue(raw, class, pos)
	if err != nil {
		return err
	}
	return v.checkValue(value, s, pos)
}

// checkValue applies the surface's evaluation environment to one value. A
// literal reaches no root and names no executable, so it needs no check
// here.
func (v Validation) checkValue(value *Value, s *Surface, pos Position) error {
	switch value.Form {
	case FormFrom:
		if !s.offers(value.From) {
			return newDiag(s.rootCode, s.rootLayer, pos,
				fmt.Sprintf("%q is not a root the %s surface offers", value.From, s.Name))
		}
	case FormExpr:
		return checkExpression(value.Expr, s, pos)
	case FormBin:
		if _, err := v.Executables.ResolveBin(value.Bin, v.From); err != nil {
			return err
		}
	case FormJSON:
		return v.checkOperand(value.JSON, s, pos)
	}
	return nil
}

func (v Validation) checkOperand(op *JSONOperand, s *Surface, pos Position) error {
	switch {
	case op == nil:
		return nil
	case op.Leaf != nil:
		return v.checkValue(op.Leaf, s, pos)
	case op.Object != nil:
		for _, key := range sortedOperandKeys(op.Object) {
			if err := v.checkOperand(op.Object[key], s, childPos(pos, key)); err != nil {
				return err
			}
		}
	default:
		for i, child := range op.Array {
			if err := v.checkOperand(child, s, childPos(pos, fmt.Sprintf("[%d]", i))); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedOperandKeys(obj map[string]*JSONOperand) []string {
	keys := make(map[string]any, len(obj))
	for k := range obj {
		keys[k] = nil
	}
	return sortedKeys(keys)
}

func table(raw any, pos Position) (map[string]any, error) {
	tbl, ok := raw.(map[string]any)
	if !ok {
		return nil, newDiag(CodeFieldType, LayerStructural, pos, "this field is a table")
	}
	return tbl, nil
}

func tableArray(body map[string]any, field string, pos Position) ([]map[string]any, error) {
	raw, ok := body[field]
	if !ok {
		return nil, nil
	}
	arr, ok := asTableArray(raw)
	if !ok {
		return nil, newDiag(CodeFieldType, LayerStructural, childPos(pos, field),
			"this field is an array of tables")
	}
	return arr, nil
}

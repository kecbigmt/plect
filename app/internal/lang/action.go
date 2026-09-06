package lang

import (
	"fmt"
	"strings"
)

// Action is one lifecycle execution, in either of the language's two
// variants. Exactly one variant's fields are populated, named by Type.
type Action struct {
	Type string

	Bin     string
	Command string
	Args    []*Value
	Stdin   *Value

	Script string
	Bind   map[string]*Value
}

// The action variants. A runtime consumer reads Type to decide whether it
// has to prepare the binding transport's private run directory, which an
// exec action needs no part of. ActionNoop is a third variant that ParseAction
// itself never recognizes: it exists only through ParseAliveAction, the one
// site where a load rule (not the shared parser) decides it is legal.
const (
	ActionExec  = "exec"
	ActionShell = "shell"
	ActionNoop  = "noop"
)

var (
	execOnlyFields  = []string{"bin", "command", "args", "stdin"}
	shellOnlyFields = []string{"script", "bind"}
)

// ParseAction reads one action. It checks the variant before anything else,
// so an action naming a type outside the vocabulary is reported as that
// rather than as a field belonging to the variant it happens to resemble.
func ParseAction(raw any, pos Position) (*Action, error) {
	tbl, ok := raw.(map[string]any)
	if !ok {
		return nil, newDiag(CodeFieldType, LayerStructural, pos, "an action is a table")
	}
	typeVal, ok := tbl["type"]
	if !ok {
		return nil, newDiag(CodeFieldRequired, LayerStructural, childPos(pos, "type"),
			"an action declares its type")
	}
	kind, _ := typeVal.(string)
	switch kind {
	case ActionExec:
		return parseExecAction(tbl, pos)
	case ActionShell:
		return parseShellAction(tbl, pos)
	default:
		return nil, newDiag(CodeActionTypeUnknown, LayerStructural, childPos(pos, "type"),
			fmt.Sprintf("an action's type is exec or shell, not %v", typeVal))
	}
}

// ParseAliveAction reads a `[health.alive]` liveness probe, the one position
// admitting noop alongside exec and shell. Every other action-bearing field
// parses through ParseAction, which does not recognize noop at all, so a
// noop misplaced anywhere else is reported the same way any other unknown
// type is: there is no second rule to state for "wrong place", only the
// ordinary vocabulary check applied at a site that never widened its
// vocabulary.
func ParseAliveAction(raw any, pos Position) (*Action, error) {
	if tbl, ok := raw.(map[string]any); ok {
		if kind, _ := tbl["type"].(string); kind == ActionNoop {
			return parseNoopAction(tbl, pos)
		}
	}
	return ParseAction(raw, pos)
}

// parseNoopAction reads a no-op action: it declares nothing beyond its type,
// runs nothing, and exits zero. It stands for a deliberate non-observation —
// RemainAfterExit=, not "no liveness condition exists" — so it carries no
// script or executable to decide never to run.
func parseNoopAction(tbl map[string]any, pos Position) (*Action, error) {
	if err := rejectUnknownFields(tbl, pos, "type"); err != nil {
		return nil, err
	}
	return &Action{Type: ActionNoop}, nil
}

func parseExecAction(tbl map[string]any, pos Position) (*Action, error) {
	if err := rejectVariantFields(tbl, pos, ActionExec, shellOnlyFields); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(tbl, pos, "type", "bin", "command", "args", "stdin"); err != nil {
		return nil, err
	}
	a := &Action{Type: ActionExec}
	_, hasBin := tbl["bin"]
	command, hasCommand := tbl["command"]
	if hasBin == hasCommand {
		return nil, newDiag(CodeActionBinAndCommand, LayerStructural, pos,
			"an exec action names its executable exactly once, through bin or command")
	}
	if hasBin {
		bin, err := tagString(tbl, "bin", childPos(pos, "bin"))
		if err != nil {
			return nil, err
		}
		a.Bin = bin
	} else {
		name, ok := command.(string)
		if !ok {
			return nil, newDiag(CodeRefDynamic, LayerStructural, childPos(pos, "command"),
				"an exec action's command is a literal OS command name or path, never a computed value")
		}
		a.Command = name
	}
	if argsVal, ok := tbl["args"]; ok {
		args, ok := argsVal.([]any)
		if !ok {
			return nil, newDiag(CodeFieldType, LayerStructural, childPos(pos, "args"), "args is an array")
		}
		for i, raw := range args {
			v, err := ParseValue(raw, ClassBinding, childPos(childPos(pos, "args"), fmt.Sprintf("[%d]", i)))
			if err != nil {
				return nil, err
			}
			a.Args = append(a.Args, v)
		}
	}
	if stdinVal, ok := tbl["stdin"]; ok {
		v, err := ParseValue(stdinVal, ClassBinding, childPos(pos, "stdin"))
		if err != nil {
			return nil, err
		}
		a.Stdin = v
	}
	return a, nil
}

func parseShellAction(tbl map[string]any, pos Position) (*Action, error) {
	if err := rejectVariantFields(tbl, pos, ActionShell, execOnlyFields); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(tbl, pos, "type", "script", "bind"); err != nil {
		return nil, err
	}
	a := &Action{Type: ActionShell}
	if _, ok := tbl["script"]; !ok {
		return nil, newDiag(CodeFieldRequired, LayerStructural, childPos(pos, "script"),
			"a shell action declares its script")
	}
	script, err := tagString(tbl, "script", childPos(pos, "script"))
	if err != nil {
		return nil, err
	}
	if strings.Contains(script, "{{") {
		return nil, newDiag(CodeShellInterpolation, LayerStructural, childPos(pos, "script"),
			"shell source is literal; an action declares the values it needs in its bind table instead")
	}
	a.Script = script
	if bindVal, ok := tbl["bind"]; ok {
		bind, ok := bindVal.(map[string]any)
		if !ok {
			return nil, newDiag(CodeFieldType, LayerStructural, childPos(pos, "bind"), "bind is a table")
		}
		a.Bind = make(map[string]*Value, len(bind))
		for name, raw := range bind {
			v, err := ParseValue(raw, ClassBinding, childPos(childPos(pos, "bind"), name))
			if err != nil {
				return nil, err
			}
			a.Bind[name] = v
		}
	}
	return a, nil
}

func rejectVariantFields(tbl map[string]any, pos Position, kind string, foreign []string) error {
	for _, field := range foreign {
		if _, ok := tbl[field]; ok {
			return newDiag(CodeActionVariant, LayerStructural, childPos(pos, field),
				fmt.Sprintf("%s belongs to the other action variant, not to %s", field, kind))
		}
	}
	return nil
}

func rejectUnknownFields(tbl map[string]any, pos Position, allowed ...string) error {
	ok := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		ok[field] = true
	}
	for field := range tbl {
		if !ok[field] {
			return newDiag(CodeFieldUnknown, LayerStructural, childPos(pos, field),
				fmt.Sprintf("%q is not part of this surface", field))
		}
	}
	return nil
}

// sortedBindKeys orders a bind table's keys, so a walk over an action's
// values reports the same diagnostic on every run and a generated binding
// file has stable contents.
func sortedBindKeys(bind map[string]*Value) []string {
	keys := make(map[string]any, len(bind))
	for k := range bind {
		keys[k] = nil
	}
	return sortedKeys(keys)
}

// values returns every value this action carries, so one walk can validate
// argv, stdin, and bindings alike.
func (a *Action) values() []*Value {
	out := make([]*Value, 0, len(a.Args)+len(a.Bind)+1)
	out = append(out, a.Args...)
	if a.Stdin != nil {
		out = append(out, a.Stdin)
	}
	for _, name := range sortedBindKeys(a.Bind) {
		out = append(out, a.Bind[name])
	}
	return out
}

// Source renders an action back to a short human-readable form, for a report
// that shows what ran rather than running it. A shell action's source is its
// script's first line, since a report line has room for no more.
func (a *Action) Source() string {
	if a == nil {
		return ""
	}
	if a.Type == ActionNoop {
		return "noop"
	}
	if a.Type == ActionShell {
		line, _, _ := strings.Cut(strings.TrimSpace(a.Script), "\n")
		return line
	}
	name := a.Command
	if a.Bin != "" {
		name = a.Bin
	}
	words := make([]string, 0, len(a.Args)+1)
	words = append(words, name)
	for _, arg := range a.Args {
		// A literal argv word reads as the word, not as the TOML string it
		// was written as: this line stands for a command being run.
		if arg.Form == FormLiteral {
			words = append(words, fmt.Sprintf("%v", arg.Literal))
			continue
		}
		words = append(words, arg.Source())
	}
	return strings.Join(words, " ")
}

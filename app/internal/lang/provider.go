package lang

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// providerContracts checks the two provider rules that read one part of a
// declaration against another part of the same declaration: a session name is
// built from captures its own regular expression declares, and a cleanup
// action reads outputs its own contract declares. Both are
// PLECTURE-CFG-FROM-PATH — the root exists, the key inside it does not.
//
// Neither can be a surface check, because a surface knows which roots a site
// observes and nothing about what this provider's regular expression captures
// or its schema declares. It runs inside ValidateDefinition rather than as a
// pass of its own so no consumer can load a provider without it.
func (v Validation) providerContracts(def *Definition, pos Position) error {
	captures, known, err := matchCaptures(def, pos)
	if err != nil {
		return err
	}
	if name, ok := def.Body["name"]; ok && known {
		value, err := ParseValue(name, ClassData, childPos(pos, "name"))
		if err != nil {
			return err
		}
		for _, path := range valuePaths(value, surfaceProviderName) {
			if err := resolveCapture(path, captures, childPos(pos, "name")); err != nil {
				return err
			}
		}
	}
	if raw, ok := def.Body["cleanup"]; ok {
		action, err := ParseAction(raw, childPos(pos, "cleanup"))
		if err != nil {
			return err
		}
		outputs, err := contractProperties(def, "outputs_schema", pos)
		if err != nil {
			return err
		}
		for _, value := range action.values() {
			for _, path := range valuePaths(value, surfaceProviderCleanup) {
				if err := resolveOutput(path, outputs, childPos(pos, "cleanup")); err != nil {
					return err
				}
			}
		}
	}
	if health, ok := def.Body["health"].(map[string]any); ok {
		if raw, ok := health["alive"]; ok {
			action, err := ParseAliveAction(raw, childPos(childPos(pos, "health"), "alive"))
			if err != nil {
				return err
			}
			outputs, err := contractProperties(def, "outputs_schema", pos)
			if err != nil {
				return err
			}
			for _, value := range action.values() {
				for _, path := range valuePaths(value, surfaceProviderHealth) {
					if err := resolveOutput(path, outputs, childPos(childPos(pos, "health"), "alive")); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// matchCaptures reads the names a provider's regular expression captures.
// known is false for an expression that does not compile: reporting every
// capture reference as unresolvable would bury the reason none of them can
// be resolved.
func matchCaptures(def *Definition, pos Position) (captures map[string]bool, known bool, err error) {
	raw, ok := def.Body["match"]
	if !ok {
		return nil, true, nil
	}
	pattern, ok := raw.(string)
	if !ok {
		return nil, false, newDiag(CodeFieldType, LayerStructural, childPos(pos, "match"),
			"match is a regular expression string")
	}
	re, compileErr := regexp.Compile(pattern)
	if compileErr != nil {
		return nil, false, newDiag(CodeFieldType, LayerStructural, childPos(pos, "match"),
			fmt.Sprintf("%q does not compile as a regular expression: %s", pattern, oneLine(compileErr)))
	}
	captures = map[string]bool{}
	for _, name := range re.SubexpNames() {
		if name != "" {
			captures[name] = true
		}
	}
	return captures, true, nil
}

// contractProperties reads one contract's declared properties, from the
// inline table or from the document the `<field>_file` names. A definition
// declaring no contract declares no property, which is why a read against it
// fails rather than being waved through.
func contractProperties(def *Definition, field string, pos Position) (map[string]bool, error) {
	if raw, ok := def.Body[field]; ok {
		table, ok := raw.(map[string]any)
		if !ok {
			return nil, newDiag(CodeFieldType, LayerStructural, childPos(pos, field),
				field+" is a JSON Schema document")
		}
		return declaredProperties(table), nil
	}
	raw, ok := def.Body[field+"_file"]
	if !ok {
		return nil, nil
	}
	name, ok := raw.(string)
	if !ok {
		return nil, newDiag(CodeFieldType, LayerStructural, childPos(pos, field+"_file"),
			field+"_file is a path")
	}
	path := name
	if !filepath.IsAbs(path) {
		dir := filepath.Dir(def.File)
		// A sibling contract is looked up next to the real document, not next
		// to a symlink to it, matching how the runtime resolves it.
		if real, linkErr := filepath.EvalSymlinks(def.File); linkErr == nil {
			dir = filepath.Dir(real)
		}
		path = filepath.Join(dir, path)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return nil, fmt.Errorf("%s: %s: %w", pos, field+"_file", readErr)
	}
	var table map[string]any
	if err := json.Unmarshal(data, &table); err != nil {
		return nil, fmt.Errorf("%s: %s: %w", pos, field+"_file", err)
	}
	return declaredProperties(table), nil
}

// valuePaths yields every root path one value reads, whether it projects one
// directly or computes over several.
func valuePaths(value *Value, s *Surface) []string {
	switch value.Form {
	case FormFrom:
		return []string{value.From}
	case FormExpr:
		return expressionPaths(value.Expr, s)
	case FormJSON:
		return operandPaths(value.JSON, s)
	default:
		return nil
	}
}

func operandPaths(op *JSONOperand, s *Surface) []string {
	switch {
	case op == nil:
		return nil
	case op.Leaf != nil:
		return valuePaths(op.Leaf, s)
	case op.Object != nil:
		var paths []string
		for _, key := range sortedOperandKeys(op.Object) {
			paths = append(paths, operandPaths(op.Object[key], s)...)
		}
		return paths
	default:
		var paths []string
		for _, child := range op.Array {
			paths = append(paths, operandPaths(child, s)...)
		}
		return paths
	}
}

func resolveCapture(path string, captures map[string]bool, pos Position) error {
	segments := strings.Split(path, ".")
	if len(segments) < 2 || segments[0] != "match" {
		return nil
	}
	if captures[segments[1]] {
		return nil
	}
	return newDiag(CodeFromPath, LayerSemantic, pos,
		fmt.Sprintf("%q names no capture this provider's match declares", path))
}

func resolveOutput(path string, outputs map[string]bool, pos Position) error {
	segments := strings.Split(path, ".")
	if len(segments) < 3 || segments[0] != "self" || segments[1] != "outputs" {
		return nil
	}
	if outputs[segments[2]] {
		return nil
	}
	return newDiag(CodeFromPath, LayerSemantic, pos,
		fmt.Sprintf("%q names no property this provider's outputs_schema declares", path))
}

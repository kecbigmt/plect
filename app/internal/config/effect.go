package config

import (
	"fmt"
	"sort"

	"github.com/kecbigmt/plecture/app/internal/lang"
)

// effectFromDefinition checks one discovered effect declaration against the
// effect surface and reads the fields the runtime needs off it.
func (c *Config) effectFromDefinition(def *lang.Definition, fromPlugin bool) (TaskDefinition, error) {
	validation := lang.Validation{
		From:        lang.Ownership{IsPlugin: fromPlugin},
		Executables: c.binResolver(def.File),
	}
	if err := validation.ValidateDefinition(def); err != nil {
		return TaskDefinition{}, err
	}
	effect, err := effectFrom(def, def.File, fromPlugin)
	if err != nil {
		return TaskDefinition{}, fmt.Errorf("effect %s in %s: %w", def.ID, def.File, err)
	}
	return effect, nil
}

// effectFrom reads the fields the runtime needs off a validated declaration.
func effectFrom(def *lang.Definition, path string, fromPlugin bool) (TaskDefinition, error) {
	pos := lang.Position{File: path, Path: def.ID}
	d := TaskDefinition{
		ID:         def.ID,
		SourcePath: path,
		BaseDir:    configFileDir(path),
		FromPlugin: fromPlugin,
	}
	if raw, ok := def.Body["scope"]; ok {
		scope, ok := raw.(string)
		if !ok {
			return d, fmt.Errorf("`scope` is a string")
		}
		d.Scope = scope
	}
	var err error
	if d.Setup, err = actionField(def, path, "setup"); err != nil {
		return d, err
	}
	if d.Cleanup, err = actionField(def, path, "cleanup"); err != nil {
		return d, err
	}
	if d.Health, err = healthFrom(def, path); err != nil {
		return d, err
	}
	if err := d.Health.Validate(); err != nil {
		return d, err
	}
	if d.Terminal, err = terminalFrom(def, path); err != nil {
		return d, err
	}
	if !d.Terminal.IsDeclared() {
		// A bare `[terminal]` header with every verb left out carries no
		// obligation, and must not read as "this effect owns the plan's
		// interactive endpoint" downstream.
		d.Terminal = nil
	}
	if err := d.readInner(def, pos); err != nil {
		return d, err
	}
	if err := d.readOutputsBind(def, pos); err != nil {
		return d, err
	}
	for _, field := range []struct {
		key    string
		schema *map[string]any
		file   *string
	}{
		{"inputs_schema", &d.InputsSchema, &d.InputsSchemaFile},
		{"outputs_schema", &d.OutputsSchema, &d.OutputsSchemaFile},
		{"locals_schema", &d.LocalsSchema, &d.LocalsSchemaFile},
	} {
		if raw, ok := def.Body[field.key]; ok {
			schema, ok := raw.(map[string]any)
			if !ok {
				return d, fmt.Errorf("`%s` is a JSON Schema document", field.key)
			}
			*field.schema = schema
		}
		if raw, ok := def.Body[field.key+"_file"]; ok {
			file, ok := raw.(string)
			if !ok {
				return d, fmt.Errorf("`%s_file` is a path", field.key)
			}
			*field.file = file
		}
	}
	return d, nil
}

func (d *TaskDefinition) readInner(def *lang.Definition, pos lang.Position) error {
	raw, ok := def.Body["inner"]
	if !ok {
		return nil
	}
	tbl, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("`inner` is a table")
	}
	uses, ok := tbl["uses"].(string)
	if !ok {
		return fmt.Errorf("`inner` names the effect it wraps through `uses`")
	}
	d.Inner = uses
	var err error
	at := childPosition(pos, "inner")
	if d.InnerInputs, err = valueTableFrom(tbl, "inputs", lang.ClassBinding, at); err != nil {
		return err
	}
	if d.InnerEnv, err = valueTableFrom(tbl, "env", lang.ClassBinding, at); err != nil {
		return err
	}
	return nil
}

func (d *TaskDefinition) readOutputsBind(def *lang.Definition, pos lang.Position) error {
	raw, ok := def.Body["outputs"]
	if !ok {
		return nil
	}
	tbl, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("`outputs` is a table")
	}
	bind, err := valueTableFrom(tbl, "bind", lang.ClassBinding, childPosition(pos, "outputs"))
	if err != nil {
		return err
	}
	d.OutputsBind = bind
	return nil
}

func valueTableFrom(body map[string]any, field string, class lang.ValueClass, pos lang.Position) (map[string]*lang.Value, error) {
	raw, ok := body[field]
	if !ok {
		return nil, nil
	}
	tbl, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("`%s` is a table", field)
	}
	at := childPosition(pos, field)
	out := make(map[string]*lang.Value, len(tbl))
	for key, entry := range tbl {
		value, err := lang.ParseValue(entry, class, childPosition(at, key))
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

func healthFrom(def *lang.Definition, path string) (*HealthConfig, error) {
	raw, ok := def.Body["health"]
	if !ok {
		return nil, nil
	}
	tbl, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("`health` is a table")
	}
	health := &HealthConfig{}
	// alive is the one probe admitting the noop variant — the language's
	// mandatory-declaration rule has already checked which position this is,
	// so this second parse only needs to read it back, through the same
	// ParseAliveAction the validator used.
	if raw, ok := tbl["alive"]; ok {
		action, err := lang.ParseAliveAction(raw, lang.Position{File: path, Path: def.ID + ".health.alive"})
		if err != nil {
			return nil, err
		}
		health.Alive = action
	}
	activity, err := actionIn(tbl, "activity", path, def.ID+".health")
	if err != nil {
		return nil, err
	}
	health.Activity = activity
	return health, nil
}

func terminalFrom(def *lang.Definition, path string) (*TerminalConfig, error) {
	raw, ok := def.Body["terminal"]
	if !ok {
		return nil, nil
	}
	tbl, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("`terminal` is a table")
	}
	terminal := &TerminalConfig{}
	for _, verb := range []struct {
		name   string
		target **lang.Action
	}{
		{"attach", &terminal.Attach},
		{"capture", &terminal.Capture},
		{"send_text", &terminal.SendText},
		{"send_keys", &terminal.SendKeys},
		{"pid", &terminal.PID},
	} {
		action, err := actionIn(tbl, verb.name, path, def.ID+".terminal")
		if err != nil {
			return nil, err
		}
		*verb.target = action
	}
	return terminal, nil
}

func actionIn(body map[string]any, field, path, tablePath string) (*lang.Action, error) {
	raw, ok := body[field]
	if !ok {
		return nil, nil
	}
	return lang.ParseAction(raw, lang.Position{File: path, Path: tablePath + "." + field})
}

// sortedBodyKeys names a declaration's fields in a stable order, so a
// diagnostic listing several of them reads the same on every run.
func sortedBodyKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

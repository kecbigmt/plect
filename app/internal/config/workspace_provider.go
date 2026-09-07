package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/kecbigmt/plecture/app/internal/lang"
)

// WorkspaceProviderConfig is loaded from `workspaces/<id>.toml` in the
// trusted base layers (plugin dirs + global config). A workspace provider
// owns everything about a *kind of resource*'s workspace lifecycle on this
// machine — knowledge that is independent of any particular workflow:
//
//   - how resource identifiers map to session ids (the resolver: pure
//     regex + a computation over its captures, offline)
//   - how the session workspace is acquired and released for such a resource
//     (setup/cleanup — the @workflow pseudo-node scripts)
//   - the outputs contract those scripts expose (outputs_schema, incl. which
//     keys may be explicitly updated through trusted side paths)
//
// Workflows reference a workspace provider by id
// (`workspace_provider = "<id>"`) and own the task shape on top of it: task
// nodes, inputs, done_when, display.
//
// Workspace providers deliberately do NOT participate in the per-workspace-dir
// cascade: setup must be resolvable before any workspace exists, and the
// scripts are arbitrary shell, so only user/machine-owned layers may supply
// them. Within one namespace a deeper layer's same-id declaration replaces the
// shallower one, mirroring task definitions — a setup/cleanup pair is atomic,
// so "append" has no sensible meaning.
type WorkspaceProviderConfig struct {
	ID string
	// Match is a regular expression over the resource identifier; Name is
	// resolved from its named captures, which are the only root Name observes
	// because it runs before a session exists. Both or neither. A provider
	// without them serves identity workflows (the input string IS the session
	// name) and does not participate in auto-dispatch.
	Match string
	Name  *lang.Value
	// Setup acquires the session workspace: it MUST emit a JSON object with
	// the reserved `workspace_dir` key on stdout. Required — acquisition is
	// the workspace provider's reason to exist.
	Setup *lang.Action
	// Cleanup releases whatever setup acquired. It intentionally does NOT
	// delete workspace_dir unless the action says so (setup/cleanup symmetry
	// is the author's contract).
	Cleanup *lang.Action
	// Subscribe binds a session to a resource of this kind at runtime (the
	// `plect subscribe` verb), the counterpart to the dispatch-time
	// auto-subscribe. Optional: a provider without it cannot be subscribed to
	// after dispatch.
	Subscribe *lang.Action
	// Unsubscribe drops a session's binding to a resource of this kind,
	// registered by Subscribe or by a dynamic task instance's `--resource`
	// binding. Optional: a provider without it never has a binding
	// unregistered on its behalf (dropping one is then whatever else already
	// releases it, e.g. Cleanup's own unconditional unsubscribe-all).
	Unsubscribe *lang.Action
	// InputsSchema declares the author's parameters: the data-shaped values a
	// workflow may set to steer this provider's hooks without replacing the
	// file. Values arrive as literal strings from the workflow's
	// `[workspace_provider_inputs]` table.
	InputsSchema     map[string]any
	InputsSchemaFile string
	// OutputsSchema is the @workflow pseudo-node contract: what setup emits and
	// which keys trusted side paths may explicitly update (`mutable = true`).
	// `workspace_dir` is reserved always-immutable.
	OutputsSchema     map[string]any
	OutputsSchemaFile string
	BaseDir           string
	SourcePath        string
	// FromPlugin says a plugin layer wrote this definition, which is what
	// decides whether its bin references may name another plugin.
	FromPlugin bool
}

// Ownership names the layer that wrote this definition, for the reference
// rules that differ between shipped and user-authored config.
func (p WorkspaceProviderConfig) Ownership() lang.Ownership {
	return lang.Ownership{IsPlugin: p.FromPlugin}
}

// ResolvedOutputsSchemaPath joins OutputsSchemaFile with BaseDir.
func (p WorkspaceProviderConfig) ResolvedOutputsSchemaPath() string {
	return resolveSchemaPath(p.OutputsSchemaFile, p.BaseDir)
}

// ResolvedInputsSchemaPath joins InputsSchemaFile with BaseDir.
func (p WorkspaceProviderConfig) ResolvedInputsSchemaPath() string {
	return resolveSchemaPath(p.InputsSchemaFile, p.BaseDir)
}

// HasResolver reports whether the workspace provider declares the
// resource-id → session-id transform.
func (p WorkspaceProviderConfig) HasResolver() bool {
	return p.Match != ""
}

// LoadWorkspaceProviders loads every workspace provider the trusted base
// layers declare: plugin roots first, then the global config dir. A mounted
// plugin's declaration answers to its catalog address, so a same-id global
// declaration is a second address rather than a replacement, and two plugins
// may each declare the id (see loadTrustedKind).
func (c *Config) LoadWorkspaceProviders() (map[string]WorkspaceProviderConfig, error) {
	resolved, err := c.trustedKind(lang.KindWorkspaceProvider)
	if err != nil {
		return nil, err
	}
	return loadTrustedKind(resolved, c.workspaceProviderFromDefinition)
}

func (c *Config) workspaceProviderFromDefinition(def *lang.Definition, fromPlugin bool) (WorkspaceProviderConfig, error) {
	validation := lang.Validation{
		From:        lang.Ownership{IsPlugin: fromPlugin},
		Executables: c.binResolver(def.File),
	}
	if err := validation.ValidateDefinition(def); err != nil {
		return WorkspaceProviderConfig{}, err
	}
	prov, err := workspaceProviderFrom(def, def.File, fromPlugin)
	if err != nil {
		return WorkspaceProviderConfig{}, fmt.Errorf("workspace provider %s in %s: %w", def.ID, def.File, err)
	}
	return prov, nil
}

// workspaceProviderFrom reads the fields the runtime needs off a validated
// declaration. The resolver pair's togetherness, setup's requiredness, and
// the reserved-key rule stay here rather than in the language validator, for
// the reason resourceDefFrom states.
func workspaceProviderFrom(def *lang.Definition, path string, fromPlugin bool) (WorkspaceProviderConfig, error) {
	pos := lang.Position{File: path, Path: def.ID}
	p := WorkspaceProviderConfig{
		ID:         def.ID,
		SourcePath: path,
		BaseDir:    configFileDir(path),
		FromPlugin: fromPlugin,
	}
	if raw, ok := def.Body["match"]; ok {
		match, ok := raw.(string)
		if !ok {
			return p, fmt.Errorf("`match` is a regular expression string")
		}
		p.Match = match
	}
	if raw, ok := def.Body["name"]; ok {
		name, err := lang.ParseValue(raw, lang.ClassData, childPosition(pos, "name"))
		if err != nil {
			return p, err
		}
		p.Name = name
	}
	if (p.Match == "") != (p.Name == nil) {
		return p, fmt.Errorf("`match` and `name` must be declared together (the resolver pair) or not at all")
	}
	var err error
	if p.Setup, err = actionField(def, path, "setup"); err != nil {
		return p, err
	}
	if p.Setup == nil {
		return p, fmt.Errorf("`setup` is required: a workspace provider's purpose is acquiring the workspace")
	}
	if p.Cleanup, err = actionField(def, path, "cleanup"); err != nil {
		return p, err
	}
	if p.Subscribe, err = actionField(def, path, "subscribe"); err != nil {
		return p, err
	}
	if p.Unsubscribe, err = actionField(def, path, "unsubscribe"); err != nil {
		return p, err
	}
	for _, field := range []struct {
		key    string
		schema *map[string]any
		file   *string
	}{
		{"inputs_schema", &p.InputsSchema, &p.InputsSchemaFile},
		{"outputs_schema", &p.OutputsSchema, &p.OutputsSchemaFile},
	} {
		if raw, ok := def.Body[field.key]; ok {
			schema, ok := raw.(map[string]any)
			if !ok {
				return p, fmt.Errorf("`%s` is a JSON Schema document", field.key)
			}
			*field.schema = schema
		}
		if raw, ok := def.Body[field.key+"_file"]; ok {
			file, ok := raw.(string)
			if !ok {
				return p, fmt.Errorf("`%s_file` is a path", field.key)
			}
			*field.file = file
		}
	}
	if err := rejectMutableWorkspaceDir(p.OutputsSchema, p.ResolvedOutputsSchemaPath()); err != nil {
		return p, err
	}
	return p, nil
}

// rejectMutableWorkspaceDir fails the load when an outputs schema declares
// the reserved `workspace_dir` key mutable — cleanup correctness depends on
// it, so no external updater may ever rewrite it. Duplicated from the task
// package's MutableOutputKeys check (config cannot import task) so the error
// fires at load time rather than first set-output.
func rejectMutableWorkspaceDir(inline map[string]any, filePath string) error {
	raw := inline
	if len(raw) == 0 && filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("read %s: %w", filePath, err)
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse %s: %w", filePath, err)
		}
	}
	props, _ := raw["properties"].(map[string]any)
	if prop, ok := props["workspace_dir"].(map[string]any); ok {
		if b, ok := prop["mutable"].(bool); ok && b {
			return fmt.Errorf("output key \"workspace_dir\" is reserved and always immutable; remove `mutable = true`")
		}
	}
	return nil
}

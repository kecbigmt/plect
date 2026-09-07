package effect

import (
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Layer is one step of a nesting chain after compilation. The layers of a
// node are homogeneous — each is an effect's own setup/cleanup pair plus the
// joint wiring it declares toward the layer it wraps — so a chain walk
// handles them without knowing which one came from a plugin and which from
// user config.
type Layer struct {
	EffectID   string
	Setup      *lang.Action
	Cleanup    *lang.Action
	SourcePath string
	From       lang.Ownership
	// InnerInputs / InnerEnv are this layer's joint toward the next layer
	// inward: the values producing that layer's input object, and the process
	// environment added to every execution inside this layer.
	InnerInputs map[string]*lang.Value
	InnerEnv    map[string]*lang.Value
	// BindOutputs is this layer's joint outward: the classified
	// `[outputs.bind]` entries that project the layer inside it into this
	// layer's own public contract.
	BindOutputs []config.OutputBinding
	// InputsSchema validates the inputs this layer is set up with, and
	// LocalsSchema the intermediates its setup emits. OutputsSchema is set
	// only for the innermost layer, whose setup emits the chain's actual
	// task outputs rather than locals.
	InputsSchema  *jsonschema.Schema
	LocalsSchema  *jsonschema.Schema
	OutputsSchema *jsonschema.Schema
	// Health and Terminal are what this layer declares for itself. They
	// compose across the chain rather than override: alive by AND, activity
	// by OR, and at most one [terminal] per chain.
	Health   *config.HealthConfig
	Terminal *config.TerminalConfig
}

// ResolveLayers compiles the schemas and output bindings of every layer of
// def's nesting chain, outermost first. Returns nil for a plain task.
func ResolveLayers(def config.TaskDefinition) ([]Layer, error) {
	if !def.IsNested() {
		return nil, nil
	}
	defs := append([]config.TaskDefinition{def}, def.InnerChain...)
	out := make([]Layer, 0, len(defs))
	for i, d := range defs {
		layer := Layer{
			EffectID:    d.ID,
			Setup:       d.Setup,
			Cleanup:     d.Cleanup,
			SourcePath:  d.SourcePath,
			From:        d.Ownership(),
			InnerInputs: d.InnerInputs,
			InnerEnv:    d.InnerEnv,
			Health:      d.Health,
		}
		if d.Terminal.IsDeclared() {
			layer.Terminal = d.Terminal
		}
		layer.BindOutputs = d.ClassifiedOutputBindings()
		var err error
		if layer.InputsSchema, err = lang.CompileSchema(d.InputsSchema, d.ResolvedInputsSchemaPath(), "plect:task:"+d.ID+":inputs"); err != nil {
			return nil, fmt.Errorf("layer %q: input schema: %w", d.ID, err)
		}
		if i == len(defs)-1 {
			if layer.OutputsSchema, err = lang.CompileSchema(d.OutputsSchema, d.ResolvedOutputsSchemaPath(), "plect:task:"+d.ID+":outputs"); err != nil {
				return nil, fmt.Errorf("layer %q: outputs schema: %w", d.ID, err)
			}
		} else if layer.LocalsSchema, err = lang.CompileSchema(d.LocalsSchema, d.ResolvedLocalsSchemaPath(), "plect:task:"+d.ID+":locals"); err != nil {
			return nil, fmt.Errorf("layer %q: locals schema: %w", d.ID, err)
		}
		out = append(out, layer)
	}
	return out, nil
}

// CleanupLayers builds the cleanup-relevant layer chain of a definition,
// skipping the schema compilation ResolveLayers does. Teardown must stay
// resilient to a definition whose config drifted to invalid after the
// instance was created, so it takes only what unwinding needs: each layer's
// cleanup, the file it came from, and the outward joint. The joint is not
// optional here — a layer's cleanup reads its own public contract, and that
// contract exists only because `[outputs.bind]` projects it, so a chain
// rebuilt without the bindings could never release what a layer produced as
// a private local. Classifying them needs no schema.
func CleanupLayers(def config.TaskDefinition) []Layer {
	if !def.IsNested() {
		return nil
	}
	defs := append([]config.TaskDefinition{def}, def.InnerChain...)
	out := make([]Layer, 0, len(defs))
	for _, d := range defs {
		out = append(out, Layer{
			EffectID:    d.ID,
			Cleanup:     d.Cleanup,
			SourcePath:  d.SourcePath,
			From:        d.Ownership(),
			BindOutputs: d.ClassifiedOutputBindings(),
		})
	}
	return out
}

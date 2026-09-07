package effect

import (
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// TestRetainLayerCleanup_OmitsCompiledSchemaFields proves the retained
// contract round-trips through JSON even when the source Layer carries
// compiled *jsonschema.Schema values RunSetup's own ResolveLayers would set
// -- RetainLayerCleanup only ever copies the schema-free fields
// CleanupLayers itself builds, so those compiled fields never need to be
// (and cannot be) serialized.
func TestRetainLayerCleanup_OmitsCompiledSchemaFields(t *testing.T) {
	schema, err := lang.CompileSchema(map[string]any{"type": "object"}, "", "plect:test")
	if err != nil {
		t.Fatalf("CompileSchema: %v", err)
	}
	layer := Layer{
		EffectID:    "outer",
		Cleanup:     &lang.Action{Type: lang.ActionShell, Script: "outer-cleanup"},
		SourcePath:  "/plugins/acme/tasks/outer.toml",
		From:        lang.Ownership{IsPlugin: true, Alias: "acme"},
		BindOutputs: []config.OutputBinding{{Key: "pid", InnerKey: "pid", Direct: true}},
		// A real setup-time Layer (effect.ResolveLayers) has these compiled;
		// RetainLayerCleanup must not choke on them, and must not attempt to
		// serialize them.
		InputsSchema: schema,
	}

	raw := RetainLayerCleanup(layer)
	if len(raw) == 0 {
		t.Fatal("RetainLayerCleanup returned nil for a layer with cleanup")
	}
	rc, ok, err := DecodeRetainedLayerCleanup(raw)
	if err != nil {
		t.Fatalf("DecodeRetainedLayerCleanup: %v", err)
	}
	if !ok {
		t.Fatal("DecodeRetainedLayerCleanup: ok = false")
	}
	if rc.EffectID != "outer" || rc.Cleanup == nil || rc.Cleanup.Script != "outer-cleanup" {
		t.Fatalf("decoded = %+v", rc)
	}
	if rc.SourcePath != layer.SourcePath || rc.From != layer.From {
		t.Fatalf("decoded source/ownership = %q/%+v, want %q/%+v", rc.SourcePath, rc.From, layer.SourcePath, layer.From)
	}
	if len(rc.BindOutputs) != 1 || rc.BindOutputs[0].Key != "pid" {
		t.Fatalf("decoded BindOutputs = %+v", rc.BindOutputs)
	}
}

// TestRetainLayerCleanup_NoCleanupReturnsNil proves a layer declaring no
// cleanup retains nothing, matching CleanupLayers' own convention of a nil
// Cleanup action for such a layer.
func TestRetainLayerCleanup_NoCleanupReturnsNil(t *testing.T) {
	if got := RetainLayerCleanup(Layer{EffectID: "outer"}); got != nil {
		t.Fatalf("RetainLayerCleanup = %s, want nil", got)
	}
}

// TestLayersFromRetained_ReconstructsCleanupOnlyChain proves the retained
// per-layer records round-trip into the same schema-free shape
// CleanupLayers builds from live config, in order.
func TestLayersFromRetained_ReconstructsCleanupOnlyChain(t *testing.T) {
	states := []contract.LayerState{
		{EffectID: "outer", Cleanup: RetainLayerCleanup(Layer{EffectID: "outer", Cleanup: &lang.Action{Type: lang.ActionShell, Script: "outer-cleanup"}})},
		{EffectID: "inner", Cleanup: RetainLayerCleanup(Layer{EffectID: "inner", Cleanup: &lang.Action{Type: lang.ActionShell, Script: "inner-cleanup"}})},
	}
	layers, ok := LayersFromRetained(states)
	if !ok {
		t.Fatal("LayersFromRetained: ok = false")
	}
	if len(layers) != 2 || layers[0].Cleanup.Script != "outer-cleanup" || layers[1].Cleanup.Script != "inner-cleanup" {
		t.Fatalf("layers = %+v", layers)
	}
}

// TestLayersFromRetained_FallsBackWholesaleWhenAnyLayerLacksRetention
// proves a mixed chain (one layer with a retained contract, one without --
// a pre-this-change execution, say) reports ok=false rather than a partial
// result, so the caller falls back to re-resolving the whole chain from the
// current definition instead of mixing retained and re-resolved layers.
func TestLayersFromRetained_FallsBackWholesaleWhenAnyLayerLacksRetention(t *testing.T) {
	states := []contract.LayerState{
		{EffectID: "outer", Cleanup: RetainLayerCleanup(Layer{EffectID: "outer", Cleanup: &lang.Action{Type: lang.ActionShell, Script: "outer-cleanup"}})},
		{EffectID: "inner"}, // no retained contract
	}
	if _, ok := LayersFromRetained(states); ok {
		t.Fatal("LayersFromRetained: ok = true, want false for a partially-retained chain")
	}
}

package effect

import (
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// A compiled InputsSchema (as a real setup-time Layer carries) must not
// break or leak into RetainLayerCleanup's output.
func TestRetainLayerCleanup_OmitsCompiledSchemaFields(t *testing.T) {
	schema, err := lang.CompileSchema(map[string]any{"type": "object"}, "", "plect:test")
	if err != nil {
		t.Fatalf("CompileSchema: %v", err)
	}
	layer := Layer{
		EffectID:     "outer",
		Cleanup:      &lang.Action{Type: lang.ActionShell, Script: "outer-cleanup"},
		SourcePath:   "/plugins/acme/tasks/outer.toml",
		From:         lang.Ownership{IsPlugin: true, Alias: "acme"},
		BindOutputs:  []config.OutputBinding{{Key: "pid", InnerKey: "pid", Direct: true}},
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

func TestRetainLayerCleanup_NoCleanupReturnsNil(t *testing.T) {
	if got := RetainLayerCleanup(Layer{EffectID: "outer"}); got != nil {
		t.Fatalf("RetainLayerCleanup = %s, want nil", got)
	}
}

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

func TestLayersFromRetained_FallsBackWholesaleWhenAnyLayerLacksRetention(t *testing.T) {
	states := []contract.LayerState{
		{EffectID: "outer", Cleanup: RetainLayerCleanup(Layer{EffectID: "outer", Cleanup: &lang.Action{Type: lang.ActionShell, Script: "outer-cleanup"}})},
		{EffectID: "inner"}, // no retained contract
	}
	if _, ok := LayersFromRetained(states); ok {
		t.Fatal("LayersFromRetained: ok = true, want false for a partially-retained chain")
	}
}

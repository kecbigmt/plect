package lang

import (
	"sort"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
)

// Surface is one row of the per-surface root table in
// docs/language/values.md: the evaluation environment every value at that
// location is validated against. A surface exposes only the context it is
// allowed to observe, rather than receiving everything and relying on a
// later check to reject the rest.
type Surface struct {
	Name  string
	roots []rootPath
	// rootLayer is the layer a foreign root is reported at here.
	// PLECTURE-CFG-FROM-ROOT is structural where the surface's roots are a
	// fixed prefix set the structural schema itself pins, and semantic
	// otherwise.
	rootLayer Layer
	// rootCode is the diagnostic a foreign root is reported as. A channel's
	// timeout has its own code, because excluding event data there is a
	// delivery-security rule rather than one row of the root table.
	rootCode Code

	envOnce  sync.Once
	envValue *cel.Env
	envErr   error
}

// rootPath is one documented root, where `<...>` stands for one segment and
// `*` for the root itself and anything under it — a channel body serializes
// the whole event, so `event.*` has to cover `event` as well.
type rootPath struct{ segments []string }

func newSurface(name string, layer Layer, roots ...string) *Surface {
	s := &Surface{Name: name, rootLayer: layer, rootCode: CodeFromRoot}
	for _, r := range roots {
		s.roots = append(s.roots, rootPath{segments: strings.Split(r, ".")})
	}
	return s
}

// offers reports whether path names a root this surface observes. A path
// running deeper than the root it matches is offered: whether the resolved
// contract declares that field is PLECTURE-CFG-FROM-PATH's question.
func (s *Surface) offers(path string) bool {
	segments := strings.Split(path, ".")
	for _, root := range s.roots {
		if root.matches(segments) {
			return true
		}
	}
	return false
}

func (r rootPath) matches(segments []string) bool {
	for i, want := range r.segments {
		if want == "*" {
			return true
		}
		if i >= len(segments) {
			return false
		}
		if strings.HasPrefix(want, "<") {
			continue
		}
		if segments[i] != want {
			return false
		}
	}
	return len(segments) >= len(r.segments)
}

// identifiers returns the variables a CEL expression at this site may name.
func (s *Surface) identifiers() []string {
	seen := map[string]bool{}
	var out []string
	for _, root := range s.roots {
		head := root.segments[0]
		if !seen[head] {
			seen[head] = true
			out = append(out, head)
		}
	}
	sort.Strings(out)
	return out
}

func withRootCode(s *Surface, code Code) *Surface {
	s.rootCode = code
	return s
}

// The surfaces below are values.md's table, in its order. A projection
// naming a root the surface does not observe is PLECTURE-CFG-FROM-ROOT.
var (
	surfaceProviderName = newSurface("workspace_provider.name", LayerSemantic,
		"match.<capture>")
	surfaceProviderSetup = newSurface("workspace_provider.setup", LayerSemantic,
		"resource.id", "session.name", "session.inputs.<key>", "inputs.<key>",
		"prev.<key>", "config.workspace_dirs_root")
	surfaceProviderCleanup = newSurface("workspace_provider.cleanup", LayerSemantic,
		"self.outputs.<key>", "inputs.<key>", "cleanup.inputs.<key>", "session.name",
		"config.workspace_dirs_root", "force")
	surfaceProviderSubscribe = newSurface("workspace_provider.subscribe", LayerSemantic,
		"session.name", "session.branch", "resource.id")
	surfaceProviderUnsubscribe = newSurface("workspace_provider.unsubscribe", LayerSemantic,
		"session.name", "resource.id")

	surfaceObserverObserve = newSurface("resource_observer.observe", LayerSemantic,
		"resource.id", "workspace.dir", "workspace.branch")
	surfaceObserverFinalize = newSurface("resource_observer.finalize", LayerSemantic,
		"resource.id", "session.name", "resource.revision", "judges")
	surfaceObserverQuery = newSurface("resource_observer.query", LayerSemantic,
		"inputs.<key>")

	surfaceWorkflowDisplay = newSurface("workflow.display", LayerSemantic,
		"workflow.outputs.<key>", "session.inputs.<key>")
	// A workflow event channel's inputs read the same roots a node's do.
	surfaceWorkflowNodeInputs = newSurface("workflow.node.inputs", LayerSemantic,
		"nodes.<id>.outputs.<key>", "workflow.outputs.<key>", "session.*",
		"session.inputs.<key>", "workspace.*")
	surfaceWorkflowPopulationInputs = newSurface("workflow.population.session.inputs", LayerSemantic,
		"resource.id", "item.<key>")

	surfaceChannelDelivery = newSurface("channel.delivery", LayerSemantic,
		"event.*", "event.metadata.<key>", "inputs.<key>")
	surfaceChannelTimeout = withRootCode(newSurface("channel.timeout", LayerStructural,
		"inputs.<key>"), CodeChannelTimeoutRoot)

	surfaceEffectSetup = newSurface("effect.setup", LayerSemantic,
		"inputs.<key>", "prev.<key>", "nodes.<id>.outputs.<key>", "workflow.outputs.<key>",
		"session.*", "session.inputs.<key>", "workspace.*", "resource.id")
	surfaceEffectCleanup = newSurface("effect.cleanup", LayerSemantic,
		"self.outputs.<key>", "inputs.<key>", "nodes.<id>.outputs.<key>",
		"workflow.outputs.<key>", "session.*", "workspace.*")
	surfaceEffectHealth = newSurface("effect.health", LayerSemantic,
		"self.outputs.<key>", "inputs.<key>", "session.*", "workspace.*")
	surfaceEffectTerminal = newSurface("effect.terminal", LayerSemantic,
		"self.outputs.<key>", "session.*")
	surfaceEffectInner = newSurface("effect.inner", LayerSemantic,
		"inputs.<key>", "locals.<key>", "nodes.<id>.outputs.<key>", "workflow.outputs.<key>",
		"session.*", "workspace.*")
	surfaceEffectOutputsBind = newSurface("effect.outputs.bind", LayerStructural,
		"inner.outputs.<key>", "locals.<key>", "inputs.<key>")

	surfaceTaskCompletion = newSurface("task.done_when", LayerStructural,
		"resource.state.<key>", "self.state.<key>")
	surfaceTaskInstruction = newSurface("task.instruction", LayerSemantic,
		"resource.id", "resource.state.<key>", "self.state.<key>", "inputs.<key>",
		"session.*", "workflow.outputs.<key>")
	surfaceChainInputs = newSurface("chain.inputs", LayerSemantic,
		"task.session", "task.instance", "task.workflow", "task.done_when.pending_judge_ids",
		"resource.state.<key>", "self.state.<key>")
)

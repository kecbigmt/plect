package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/lang"
)

// discoveredLayer is one cascade layer read as a definition root: every
// declaration under it, whatever directory the author filed it in. A
// directory under a definition root is organization, not identity, so which
// kind a declaration is comes from its own `kind` field rather than from the
// folder it sits in.
type discoveredLayer struct {
	layer layerDir
	defs  []*lang.Definition
}

// A layer's trust decides what it may declare, and the three answers are not
// the same shape. A kind a layer may declare is loaded; a kind it must not
// declare is a load error, because reading it as absent would leave the author
// believing their declaration is live; and a kind that simply does not reach
// this layer is skipped, because a project's own directory holding one is not
// a mistake — it just does not participate.
//
// A workspace provider, a resource observer and a channel are trusted-base
// content: they decide how a session's identity is resolved and how a process
// is reached, so one in an ancestor overlay is skipped — it does not
// participate, and does not stop the layer loading either. Effects and task
// documents do reach an ancestor overlay, since a project may add its own
// lifecycle and its own work; they never reach the workspace directory, which
// is cloned content, so shell there or a declaration of the work the clone is
// about is refused outright.
var (
	loadedKinds = map[layerScope]map[lang.Kind]bool{
		layerScopeTrusted: {
			lang.KindWorkspaceProvider: true,
			lang.KindResourceObserver:  true,
			lang.KindChannel:           true,
			lang.KindEffect:            true,
			lang.KindTask:              true,
			lang.KindWorkflow:          true,
		},
		layerScopeOverlay: {
			lang.KindEffect:   true,
			lang.KindTask:     true,
			lang.KindWorkflow: true,
		},
		layerScopeWorkspaceDir: {
			lang.KindWorkflow: true,
		},
	}
	refusedKinds = map[layerScope]map[lang.Kind]string{
		layerScopeWorkspaceDir: {
			lang.KindEffect: "clone content must not carry shell",
			lang.KindTask:   "clone content must not declare the work it is about",
		},
	}
)

// layerScope names how far a layer's trust reaches, which is what decides
// which kinds it may declare.
type layerScope int

const (
	// layerScopeTrusted is a plugin's own config root or the machine's global
	// config: machine-owned, so anything may be declared there.
	layerScopeTrusted layerScope = iota
	// layerScopeOverlay is an ancestor `.plect/` directory above the
	// workspace directory: trusted, but a project's own.
	layerScopeOverlay
	// layerScopeWorkspaceDir is the `.plect/` directory inside the workspace
	// directory: cloned, untrusted content.
	layerScopeWorkspaceDir
)

func (l layerDir) scope() layerScope {
	switch {
	case l.workspaceDir:
		return layerScopeWorkspaceDir
	case l.plugin || l.global:
		return layerScopeTrusted
	default:
		return layerScopeOverlay
	}
}

// discoverLayers reads every cascade layer's definition root once, in
// shallowest-first order. workspaceDirPath selects the ancestor overlays; an
// empty one means the trusted base layers alone, which is what a caller
// outside any workspace directory sees.
func (c *Config) discoverLayers(workspaceDirPath string) ([]discoveredLayer, error) {
	out := make([]discoveredLayer, 0, len(c.PluginDirs)+2)
	for _, layer := range c.definitionRoots(workspaceDirPath) {
		defs, err := discoverLayer(layer)
		if err != nil {
			return nil, err
		}
		if len(defs) == 0 {
			continue
		}
		out = append(out, discoveredLayer{layer: layer, defs: defs})
	}
	return out, nil
}

func discoverLayer(layer layerDir) ([]*lang.Definition, error) {
	if _, err := os.Stat(layer.dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := checkRetiredChainsDir(layer.dir); err != nil {
		return nil, err
	}
	discovered, err := lang.DiscoverRoot(layer.dir, lang.ReservedFileNames)
	if err != nil {
		return nil, err
	}
	scope := layer.scope()
	loaded := loadedKinds[scope]
	refused := refusedKinds[scope]
	var defs []*lang.Definition
	for _, def := range discovered {
		if because, no := refused[def.Kind]; no {
			return nil, fmt.Errorf("%s: %q declares kind %q, which a `.plect/` directory inside the workspace directory may not (%s); move it to the global layer, a plugin, or an overlay above the workspace dir", def.File, def.ID, def.Kind, because)
		}
		if !loaded[def.Kind] {
			continue
		}
		if scope == layerScopeWorkspaceDir && filepath.Dir(def.File) != filepath.Join(layer.dir, "workflows") {
			// The workspace-dir layer is not a definition root: only
			// `.plect/workflows/` is read there, so the directory is the
			// allowlist rather than author organization.
			continue
		}
		defs = append(defs, def)
	}
	sort.SliceStable(defs, func(i, j int) bool { return defs[i].ID < defs[j].ID })
	return defs, nil
}

// ofKind selects one kind's declarations from a discovered layer, preserving
// the layer's own id order so a diagnostic naming the first offender is
// reproducible.
func (d discoveredLayer) ofKind(kind lang.Kind) []*lang.Definition {
	var out []*lang.Definition
	for _, def := range d.defs {
		if def.Kind == kind {
			out = append(out, def)
		}
	}
	return out
}

// checkRetiredChainsDir names the one leftover a reader would otherwise meet
// as an unexplained "not a definition table": a definition root admits no
// non-definition TOML, so a surviving `chains/*.toml` from before chains moved
// into the declaration that fires them stops the whole layer from loading.
// Reported here, with the fix, rather than left to the generic diagnostic.
func checkRetiredChainsDir(root string) error {
	entries, err := os.ReadDir(filepath.Join(root, "chains"))
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		return fmt.Errorf("%s is not a definition: declare [[chains]] inside the task document whose instances it fires against, and delete the retired chains/ directory", filepath.Join(root, "chains", entry.Name()))
	}
	return nil
}

// resolvedDefinition is one declaration after the cascade has chosen it,
// carrying the layer facts a loader needs beyond the declaration itself.
type resolvedDefinition struct {
	def        *lang.Definition
	fromPlugin bool
	pluginID   string
	// address is the name this declaration is stored and looked up under —
	// its id alone when the user owns it, and its catalog address when a
	// mounted plugin declared it.
	address string
	// prefix is what a relative reference written inside this declaration
	// resolves against, empty for a declaration the user owns.
	prefix string
	// source is the shallowest layer that declared the id, which is the file
	// a relative schema path resolves against and the layer a reference
	// written in the declaration resolves in.
	source string
}

// definitionAddress is the name a declaration is stored and referenced under.
// A mounted plugin's declaration takes its catalog address, which is what lets
// two plugins declare one id and stay tellable apart. Everything else keeps
// its bare id: the user-owned layer stack is a single namespace with no alias
// to qualify by, and a hand-authored `plugin_dirs` entry carries no catalog
// identity, so no address exists that could name it.
func definitionAddress(layer layerDir, id string) string {
	prefix := addressPrefix(layer)
	if prefix == "" {
		return id
	}
	return prefix + "." + id
}

// addressPrefix is the dotted prefix a declaration in this layer is addressed
// under, and equally the prefix a relative reference written there resolves
// against. It is empty for every layer that has no catalog identity to name it
// by, which is what makes such a layer's declarations bare-addressed.
func addressPrefix(layer layerDir) string {
	if !layer.plugin || layer.pluginID == "" {
		return ""
	}
	own := pluginOwnership(layer.pluginID)
	if own.Path == "" {
		return own.Alias
	}
	return own.Alias + "." + own.Path
}

// referenceAddress reads a reference as written where it was written and
// returns the address it selects. A relative reference inside a mounted plugin
// names that plugin's own declaration, which is why the prefix is prepended
// rather than searched for; everywhere else the reference is already the
// address, bare or catalog-qualified.
func referenceAddress(prefix, ref string) (string, error) {
	if ref == "" || prefix == "" {
		return ref, nil
	}
	if strings.Contains(ref, ".") {
		return "", fmt.Errorf("%s: a plugin-owned reference must be relative; %q names a catalog alias or another plugin's ownership segment", prefix, ref)
	}
	return prefix + "." + ref, nil
}

// resolveNamespace folds one kind's declarations across the cascade, grouped
// by the address each declaration answers to. Within one address a deeper
// user-owned layer replaces what a shallower one declared, except a workflow,
// which merges by the cascade rules.
//
// Two mounted plugins declaring one id land on different addresses and both
// stay reachable, and a user-owned declaration sharing an id with a plugin's
// no longer hides it: the two are separate addresses, so which one a reference
// selects is decided by how the reference is written rather than by layer
// depth. Two plugin layers with no catalog identity are the one collision left
// — nothing can name them apart, so choosing between them would fall to
// declaration order.
//
// The fold is per kind because id uniqueness across kinds is a rule *within* a
// layer, which discovery enforces for a whole root, and not across them: each
// plugin is its own namespace, and a reference disambiguates by the kind its
// site expects. So a plugin's `goal_review` task document and a user-owned
// `goal_review` workflow coexist — a chain naming that id wants the workflow,
// and an instantiation wants the document.
func (c *Config) resolveNamespace(layers []discoveredLayer, kind lang.Kind) ([]resolvedDefinition, error) {
	type group struct {
		defs       []*lang.Definition
		fromPlugin bool
		pluginID   string
		prefix     string
		source     string
		// unaliased names the file of the identity-less plugin layer that
		// already claimed this address, which is what the second one collides
		// with.
		unaliased string
	}
	groups := make(map[string]*group)
	var order []string
	for _, discovered := range layers {
		for _, def := range discovered.ofKind(kind) {
			address := definitionAddress(discovered.layer, def.ID)
			g, seen := groups[address]
			if !seen {
				// The shallowest declarer keeps source, so a deeper layer
				// extending a workflow does not move what its relative paths
				// resolve against.
				g = &group{source: def.File, prefix: addressPrefix(discovered.layer)}
				groups[address] = g
				order = append(order, address)
			}
			if discovered.layer.plugin && discovered.layer.pluginID == "" {
				if g.unaliased != "" {
					return nil, fmt.Errorf("id %q is declared by two plugin layers that carry no catalog identity (%s and %s); a hand-authored plugin_dirs entry has no address to tell it apart from another, so enable them through a catalog or replace one definition in global config", def.ID, g.unaliased, def.File)
				}
				g.unaliased = def.File
			}
			g.defs = append(g.defs, def)
			g.fromPlugin = discovered.layer.plugin
			g.pluginID = discovered.layer.pluginID
		}
	}
	sort.Strings(order)
	out := make([]resolvedDefinition, 0, len(order))
	for _, address := range order {
		g := groups[address]
		var merged []*lang.Definition
		for _, def := range g.defs {
			combined, err := lang.MergeLayer(merged, []*lang.Definition{def})
			if err != nil {
				return nil, err
			}
			merged = combined
		}
		out = append(out, resolvedDefinition{
			def:        merged[0],
			fromPlugin: g.fromPlugin,
			pluginID:   g.pluginID,
			address:    address,
			prefix:     g.prefix,
			source:     g.source,
		})
	}
	return out, nil
}

// canonicalizeWorkflowRefs rewrites a workflow's static references — its
// workspace provider, each node's `uses`, and each event channel's `uses` — to
// the addresses they select, so the runtime resolves topology by map lookup
// instead of re-reading the reference grammar at every site.
func canonicalizeWorkflowRefs(wf *WorkflowFile, prefix string) error {
	if prefix == "" {
		return nil
	}
	provider, err := referenceAddress(prefix, wf.WorkspaceProvider)
	if err != nil {
		return fmt.Errorf("workflow %q workspace_provider: %w", wf.ID, err)
	}
	wf.WorkspaceProvider = provider
	for i := range wf.Nodes {
		uses, err := referenceAddress(prefix, wf.Nodes[i].Uses)
		if err != nil {
			return fmt.Errorf("workflow %q node %q: %w", wf.ID, wf.Nodes[i].ID, err)
		}
		wf.Nodes[i].Uses = uses
	}
	for i := range wf.Event.Channel {
		uses, err := referenceAddress(prefix, wf.Event.Channel[i].Uses)
		if err != nil {
			return fmt.Errorf("workflow %q event channel %q: %w", wf.ID, wf.Event.Channel[i].Name, err)
		}
		wf.Event.Channel[i].Uses = uses
	}
	for i := range wf.Populations {
		observer, err := referenceAddress(prefix, wf.Populations[i].ResourceObserver)
		if err != nil {
			return fmt.Errorf("workflow %q population %q resource_observer: %w", wf.ID, wf.Populations[i].Name, err)
		}
		wf.Populations[i].ResourceObserver = observer
		task, err := referenceAddress(prefix, wf.Populations[i].Session.Task)
		if err != nil {
			return fmt.Errorf("workflow %q population %q session.task: %w", wf.ID, wf.Populations[i].Name, err)
		}
		wf.Populations[i].Session.Task = task
	}
	return nil
}

// canonicalizeDocumentRefs rewrites a task document's observer reference and
// each chain's workflow reference to the addresses they select.
func canonicalizeDocumentRefs(doc *TaskDocument, prefix string) error {
	if prefix == "" {
		return nil
	}
	observer, err := referenceAddress(prefix, doc.ResourceObserver)
	if err != nil {
		return fmt.Errorf("task %q resource_observer: %w", doc.ID, err)
	}
	doc.ResourceObserver = observer
	extends, err := referenceAddress(prefix, doc.Extends)
	if err != nil {
		return fmt.Errorf("task %q extends: %w", doc.ID, err)
	}
	doc.Extends = extends
	for i := range doc.Chains {
		workflow, err := referenceAddress(prefix, doc.Chains[i].Workflow)
		if err != nil {
			return fmt.Errorf("task %q chain %q: %w", doc.ID, doc.Chains[i].ID, err)
		}
		doc.Chains[i].Workflow = workflow
	}
	return nil
}

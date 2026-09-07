package config

import (
	"github.com/kecbigmt/plecture/app/internal/lang"
)

// loadTrustedKind selects one kind from the trusted base layers' resolved
// namespace — every plugin's config root, then the machine's global config —
// and reads each declaration with one, which turns it into the runtime's own
// shape.
//
// The per-workspace-dir ancestor cascade is deliberately excluded: a
// workspace provider, a resource observer and a channel decide how a session's
// identity is resolved and how a process is reached, and those belong to the
// machine rather than to any directory a project owns. Such a declaration in
// an ancestor overlay is skipped rather than refused, because a project's own
// directory holding one is not a mistake — it simply does not participate.
func loadTrustedKind[T any](resolved []resolvedDefinition, one func(*lang.Definition, bool) (T, error)) (map[string]T, error) {
	out := make(map[string]T)
	for _, entry := range resolved {
		loaded, err := one(entry.def, entry.fromPlugin)
		if err != nil {
			return nil, err
		}
		out[entry.address] = loaded
	}
	return out, nil
}

// trustedKind resolves one kind over the base layers alone. A caller loading a
// kind the cascade does not reach has no workspace directory to pass, and
// reading the overlays anyway would only discover declarations it must skip.
func (c *Config) trustedKind(kind lang.Kind) ([]resolvedDefinition, error) {
	layers, err := c.discoverLayers("")
	if err != nil {
		return nil, err
	}
	return c.resolveNamespace(layers, kind)
}

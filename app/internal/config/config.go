package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plugins"
)

// Duration wraps time.Duration with a TOML UnmarshalText implementation so
// users can write strings like "7d" / "30m" in config.toml.
type Duration struct {
	time.Duration
}

// UnmarshalText parses a duration string. Accepts standard Go durations plus
// the "d" (day) suffix as a convenience (7d = 168h).
func (d *Duration) UnmarshalText(text []byte) error {
	dur, err := ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

// ParseDuration is Duration's own grammar, exported for the fields that carry
// a duration as a plain string rather than as a decoded Duration (a channel's
// `timeout`, which may be a projection resolved per delivery). The empty
// string is zero, not an error, matching UnmarshalText on an absent value.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	// Translate trailing "d" (days) into hours since time.ParseDuration doesn't accept it.
	if n := len(s); n > 1 && s[n-1] == 'd' {
		if days, err := time.ParseDuration(s[:n-1] + "h"); err == nil {
			return days * 24, nil
		}
	}
	return time.ParseDuration(s)
}

// Task scope constants mirror contracts/state.TaskScope* but are duplicated
// here to avoid the config package depending on contracts/state.
const (
	TaskScopeSession = "session"
	TaskScopeRun     = "run"
)

type Config struct {
	SchemaVersion     int    `toml:"schema_version"`
	WorkspaceDirsRoot string `toml:"workspace_dirs_root"`
	// MaxUpChildren caps concurrently-up sessions whose logical parent is the
	// virtual root. Nil leaves parentless admission unlimited.
	MaxUpChildren *int `toml:"max_up_children"`
	// ResourceAllowlist is the security boundary for session creation: regex
	// patterns the resource identifier must match. The boundary exists
	// because agents (MCP) can invoke plect with arbitrary input, and a
	// session create executes trusted-layer shell against that input.
	// Only the user-owned global config can declare it. Empty means allow
	// all.
	ResourceAllowlist []string `toml:"resource_allowlist"`
	// PluginDirs lists plugin roots (each containing a config/ subdirectory
	// with workspaces/, resources/, channels/, tasks/, and workflows/
	// subdirectories — see docs/design/plugin-packaging.md's
	// Package format section). Plugins form the base cascade layer: the
	// global layer and ancestor overlays stack on top of them. Entries are
	// trusted — only the user or the system's config-management tooling writes config.toml.
	PluginDirs []string `toml:"plugin_dirs"`
	// Plugins is the resolved catalog-qualified identity + manifest for
	// every plugin folded into PluginDirs by resolveDeclaredPlugins (empty
	// for hand-authored plugin_dirs entries, which carry no catalog
	// identity). A `bin` reference resolves against this in-memory list
	// instead of re-parsing plugin.toml on every render.
	Plugins []plugins.Mounted `toml:"-"`
	// catalogRegistrations/catalogLock/catalogCacheRoot are the same local
	// state resolveDeclaredPlugins already resolved Plugins from, kept
	// around (unexported: no toml tag needed) so a bin reference's remediation
	// hint can re-derive
	// a catalog's full *published* plugin list on demand — see
	// describeMissingPlugin. nil/"" for a Config built without going through
	// Load() (e.g. tests), in which case describeMissingPlugin degrades to
	// no hint rather than erroring.
	catalogRegistrations *plugins.CatalogRegistrations
	catalogLock          *plugins.Lockfile
	catalogCacheRoot     string
	runScopeCache        atomic.Pointer[runScopeCache]
	Detached             bool           `toml:"detached"`
	Channels             []string       `toml:"channels"`
	InputsSchema         map[string]any `toml:"inputs_schema"`
	InputsSchemaFile     string         `toml:"inputs_schema_file"`
	BaseDir              string         `toml:"-"`
	// SessionGuard is a per-session dispatch boundary sourced from the
	// PLECT_SESSION_GUARD environment variable (not config.toml). When set, a
	// `plect up` may only produce a *resolved session name* that
	// matches this regex. The orchestrator's agent pane exports it from the
	// workspace provider's `session_guard` output (e.g. "^acme/"), so a
	// prompt-injected board body cannot make the orchestrator dispatch work
	// outside its own session-name space. Empty = disabled.
	//
	// The guard is intentionally an opaque regex over the resolved session
	// name: plect core stays workspace-provider-agnostic and never parses
	// the resource identifier's internal structure — knowing how names are
	// shaped is the workspace provider's job, encoded in the pattern it
	// emits.
	SessionGuard string `toml:"-"`
}

func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		SchemaVersion:     lang.KnownSchemaVersion,
		WorkspaceDirsRoot: filepath.Join(home, "workspace_dirs"),
		Detached:          true,
		SessionGuard:      os.Getenv("PLECT_SESSION_GUARD"),
	}
}

// DefaultPath returns the path Load reads config.toml from. Callers outside
// the normal load path (e.g. the migration command, which must locate the
// same file Load would) use this instead of duplicating the join.
func DefaultPath() string {
	home, err := confighome.Resolve()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "config.toml")
}

// Load reads config.toml and resolves any plugins enabled across registered
// catalogs in ~/.config/plect/catalogs.toml into cfg.PluginDirs/cfg.Plugins.
// Unlike a malformed config.toml (which degrades to defaults with a warning
// — see below), a plugin resolution failure returns a non-nil error: a
// missing or mismatched lock entry, tampered content, or an incompatible
// plugin must fail the whole load rather than silently mount nothing and
// continue.
func Load() (*Config, error) {
	cfg := DefaultConfig()

	configPath := DefaultPath()
	if configPath == "" {
		return resolveDeclaredPlugins(cfg)
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return resolveDeclaredPlugins(cfg)
	}

	meta, err := toml.DecodeFile(configPath, cfg)
	if err != nil {
		// A present but unparsable config.toml is a user mistake, not an
		// absent-config no-op: silently falling back to defaults here would
		// hide a typo behind seemingly-ignored settings, so this warns
		// (disposition: surfaced) while still returning usable defaults.
		slog.Warn("config.toml present but failed to parse; using defaults", "path", configPath, "error", err)
		return resolveDeclaredPlugins(cfg)
	}
	if !meta.IsDefined("schema_version") {
		return nil, fmt.Errorf("config.toml: schema_version is required")
	}
	if cfg.SchemaVersion != lang.KnownSchemaVersion {
		return nil, fmt.Errorf("config.toml: schema_version %d is not supported (want %d); see docs/migrations/standing-session-dispatch-dialect-migration.md", cfg.SchemaVersion, lang.KnownSchemaVersion)
	}
	if cfg.MaxUpChildren != nil && *cfg.MaxUpChildren < 1 {
		return nil, fmt.Errorf("config.toml: max_up_children must be at least 1, got %d", *cfg.MaxUpChildren)
	}
	if meta.IsDefined("worktrees_root") {
		slog.Warn("legacy config key worktrees_root is ignored; rename it to workspace_dirs_root or run the legacy migration", "path", configPath)
	}
	if meta.IsDefined("workdirs_root") {
		slog.Warn("legacy config key workdirs_root is ignored; rename it to workspace_dirs_root or run the legacy migration", "path", configPath)
	}

	// Expand ~ in path configs
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		return resolveDeclaredPlugins(cfg)
	}
	if len(cfg.WorkspaceDirsRoot) > 0 && cfg.WorkspaceDirsRoot[0] == '~' {
		cfg.WorkspaceDirsRoot = filepath.Join(home, cfg.WorkspaceDirsRoot[1:])
	}
	for i, dir := range cfg.PluginDirs {
		if len(dir) > 0 && dir[0] == '~' {
			cfg.PluginDirs[i] = filepath.Join(home, dir[1:])
		}
	}

	cfg.BaseDir = configFileDir(configPath)

	return resolveDeclaredPlugins(cfg)
}

func (c *Config) ResolvedInputsSchemaPath() string {
	if c.InputsSchemaFile == "" {
		return ""
	}
	if filepath.IsAbs(c.InputsSchemaFile) {
		return c.InputsSchemaFile
	}
	if c.BaseDir == "" {
		return c.InputsSchemaFile
	}
	return filepath.Join(c.BaseDir, c.InputsSchemaFile)
}

// configFileDir resolves symlinks so sibling files (e.g. outputs_schema_file)
// are looked up next to the real config, not next to the symlink.
func configFileDir(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Dir(real)
	}
	return filepath.Dir(path)
}

// IsSessionNameAllowed enforces the per-session SessionGuard boundary against
// a *resolved* session name. It is orthogonal to the allowlist: the allowlist
// is a static user-owned ceiling on resource identifiers, while SessionGuard
// is the dynamic per-orchestrator floor that keeps a session from dispatching
// work outside its own name space.
//
// Allow when SessionGuard is empty (disabled) or the regex matches the name.
// The guard runs on the resolver's canonical output — multiple input forms
// fold to one name, so there is no alternate-form bypass and no double parse
// of the resource identifier. An invalid pattern is an error (fail closed),
// mirroring IsResourceAllowed.
func (c *Config) IsSessionNameAllowed(sessionName string) (bool, error) {
	if c.SessionGuard == "" {
		return true, nil
	}
	re, err := regexp.Compile(c.SessionGuard)
	if err != nil {
		return false, fmt.Errorf("session guard %q: %w", c.SessionGuard, err)
	}
	return re.MatchString(sessionName), nil
}

// IsResourceAllowed checks the resource identifier against the allowlist
// boundary before dispatch. Allow when the allowlist is empty (boundary
// disabled) or any resource_allowlist regex matches.
//
// Invalid patterns surface as errors — a silently-skipped pattern would
// fail open on exactly the inputs it was meant to block.
func (c *Config) IsResourceAllowed(resource string) (bool, error) {
	if len(c.ResourceAllowlist) == 0 {
		return true, nil
	}
	for _, pat := range c.ResourceAllowlist {
		re, err := regexp.Compile(pat)
		if err != nil {
			return false, fmt.Errorf("resource_allowlist pattern %q: %w", pat, err)
		}
		if re.MatchString(resource) {
			return true, nil
		}
	}
	return false, nil
}

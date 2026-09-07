package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/testpath"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Detached != true {
		t.Errorf("Detached = %v, want true", cfg.Detached)
	}
	if cfg.ResourceAllowlist != nil {
		t.Errorf("ResourceAllowlist = %v, want nil", cfg.ResourceAllowlist)
	}
}

func TestDefaultPathUsesPlectureConfigDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	want := filepath.Join(tmpHome, ".config", "plect", "config.toml")
	if got := DefaultPath(); got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultPath_HonorsConfigHomeEnvVar(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configHome := filepath.Join(t.TempDir(), "custom-config")
	t.Setenv(confighome.EnvVar, configHome)

	want := filepath.Join(configHome, "config.toml")
	if got := DefaultPath(); got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestLoad_WithConfigFile(t *testing.T) {
	// Create a temp home directory with a config file
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	configDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	configContent := `
schema_version = 2
workspace_dirs_root = "~/my-workspace-dirs"
resource_allowlist = ["^https://example\\.test/org/", "^https://example\\.test/other/"]
detached = false
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceDirsRoot != filepath.Join(tmpHome, "my-workspace-dirs") {
		t.Errorf("WorkspaceDirsRoot = %q, want %q", cfg.WorkspaceDirsRoot, filepath.Join(tmpHome, "my-workspace-dirs"))
	}
	if len(cfg.ResourceAllowlist) != 2 {
		t.Errorf("ResourceAllowlist length = %d, want 2", len(cfg.ResourceAllowlist))
	}
	if cfg.Detached != false {
		t.Errorf("Detached = %v, want false", cfg.Detached)
	}
}

func TestLoad_NoConfigFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Should return defaults
	if cfg.Detached != true {
		t.Errorf("Detached = %v, want true", cfg.Detached)
	}
}

func TestLoad_RejectsSupersededDialect(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	configDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("schema_version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("standing-session-dispatch-dialect-migration.md")) {
		t.Fatalf("error = %v, want the dialect migration named", err)
	}
}

func TestLoad_MalformedConfigFileFallsBackAndWarns(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	configDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("not = [valid toml"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Detached != true {
		t.Errorf("Detached = %v, want true (defaults)", cfg.Detached)
	}
	if !bytes.Contains(logs.Bytes(), []byte("config.toml present but failed to parse")) {
		t.Errorf("expected a warning about the unparsable config.toml, got log output: %q", logs.String())
	}
}

func TestLoad_LegacyWorktreesRootWarns(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	configDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("schema_version = 2\nworktrees_root = \"/legacy/worktrees\""), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceDirsRoot != filepath.Join(tmpHome, "workspace_dirs") {
		t.Errorf("WorkspaceDirsRoot = %q, want default because legacy key is ignored", cfg.WorkspaceDirsRoot)
	}
	if !bytes.Contains(logs.Bytes(), []byte("legacy config key worktrees_root is ignored")) {
		t.Errorf("expected a warning about worktrees_root, got log output: %q", logs.String())
	}
}

func TestLoad_LegacyWorkdirsRootWarns(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	configDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("schema_version = 2\nworkdirs_root = \"/legacy/workdirs\""), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceDirsRoot != filepath.Join(tmpHome, "workspace_dirs") {
		t.Errorf("WorkspaceDirsRoot = %q, want default because legacy key is ignored", cfg.WorkspaceDirsRoot)
	}
	if !bytes.Contains(logs.Bytes(), []byte("legacy config key workdirs_root is ignored")) {
		t.Errorf("expected a warning about workdirs_root, got log output: %q", logs.String())
	}
}

func TestLoad_PopulatesBaseDir(t *testing.T) {
	tmpHome := testpath.Real(t, t.TempDir())
	t.Setenv("HOME", tmpHome)
	configDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("schema_version = 2\ninputs_schema_file = \"in.schema.json\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseDir != configDir {
		t.Errorf("BaseDir = %q, want %q", cfg.BaseDir, configDir)
	}
	want := filepath.Join(configDir, "in.schema.json")
	if got := cfg.ResolvedInputsSchemaPath(); got != want {
		t.Errorf("ResolvedInputsSchemaPath = %q, want %q", got, want)
	}
}

func TestLoad_ReadsFromConfigHomeEnvVarInsteadOfRealHome(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	realConfigDir := filepath.Join(realHome, ".config", "plect")
	if err := os.MkdirAll(realConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realConfigDir, "config.toml"), []byte("schema_version = 2\nworkspace_dirs_root = \"/real-home-workspace-dirs\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	overrideDir := testpath.Real(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(overrideDir, "config.toml"), []byte("schema_version = 2\nworkspace_dirs_root = \"/override-workspace-dirs\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(confighome.EnvVar, overrideDir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceDirsRoot != "/override-workspace-dirs" {
		t.Errorf("WorkspaceDirsRoot = %q, want the override dir's value, not the real home's", cfg.WorkspaceDirsRoot)
	}
	if cfg.BaseDir != overrideDir {
		t.Errorf("BaseDir = %q, want %q", cfg.BaseDir, overrideDir)
	}
}

func TestLoad_InlineInputsSchema(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	configDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgContent := `
schema_version = 2

[inputs_schema]
type = "object"
required = ["template"]

[inputs_schema.properties]
template = { type = "string" }
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InputsSchema == nil {
		t.Fatal("expected InputsSchema parsed from TOML")
	}
	if cfg.InputsSchema["type"] != "object" {
		t.Errorf("InputsSchema[type] = %v, want object", cfg.InputsSchema["type"])
	}
}

func TestResolvedInputsSchemaPath(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"empty", Config{}, ""},
		{"absolute path", Config{InputsSchemaFile: "/abs/in.json"}, "/abs/in.json"},
		{"relative with base", Config{InputsSchemaFile: "in.json", BaseDir: "/cfg"}, "/cfg/in.json"},
		{"relative without base returns as-is", Config{InputsSchemaFile: "in.json"}, "in.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.ResolvedInputsSchemaPath(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestIsResourceAllowed(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		resource string
		want     bool
		wantErr  bool
	}{
		{name: "both lists empty allows all", cfg: Config{}, resource: "anything", want: true},
		{
			name:     "resource pattern matches",
			cfg:      Config{ResourceAllowlist: []string{`^https://github\.com/org/`}},
			resource: "https://github.com/org/repo/issues/1",
			want:     true,
		},
		{
			name:     "resource pattern rejects",
			cfg:      Config{ResourceAllowlist: []string{`^https://github\.com/org/`}},
			resource: "https://github.com/evil/repo/issues/1",
			want:     false,
		},
		{
			name:     "non-github resource allowed via resource pattern",
			cfg:      Config{ResourceAllowlist: []string{`^https://jira\.example\.com/browse/`}},
			resource: "https://jira.example.com/browse/PROJ-1",
			want:     true,
		},
		{
			name:    "invalid pattern surfaces error",
			cfg:     Config{ResourceAllowlist: []string{`([`}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.IsResourceAllowed(tt.resource)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsSessionNameAllowed(t *testing.T) {
	tests := []struct {
		name    string
		guard   string
		session string
		want    bool
		wantErr bool
	}{
		{name: "empty guard allows all", guard: "", session: "exampleorg/x-26", want: true},
		{name: "owner-prefix guard matches own owner", guard: "^acme/", session: "acme/widgets-1", want: true},
		{name: "owner-prefix guard rejects other owner", guard: "^acme/", session: "exampleorg/x-26", want: false},
		{name: "tagged session still matches", guard: "^acme/", session: "acme/widgets-1+review", want: true},
		{name: "trailing slash blocks lookalike owner", guard: "^acme/", session: "acme-evil/x-1", want: false},
		{name: "invalid pattern fails closed", guard: "([", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{SessionGuard: tt.guard}
			got, err := cfg.IsSessionNameAllowed(tt.session)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("IsSessionNameAllowed(%q) with guard %q = %v, want %v", tt.session, tt.guard, got, tt.want)
			}
		})
	}
}

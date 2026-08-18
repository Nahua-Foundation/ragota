package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Nahua-Foundation/ragota/internal/config"
)

// writeConfigFile marshals a config to a temp YAML file and returns its path.
// runCheckConfig now reads the file back to check for unknown keys, so tests
// that build a config in memory must give it a real file to read, mirroring
// production where the path always points at the file the config was loaded from.
func writeConfigFile(t *testing.T, cfg *config.Config) string {
	t.Helper()
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func listenLocal(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func TestDependencyProbes_ReportsReachability(t *testing.T) {
	addr := listenLocal(t)
	cfg := &config.Config{
		Storage: config.StorageConfig{
			SQLite: &config.SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1},
			Qdrant: &config.QdrantStorageConfig{URL: "http://" + addr},
		},
		LSP: &config.LSPConfig{
			Enabled:   true,
			HostRoot:  "/srv/repos",
			MountRoot: "/workspace",
			Servers: map[string]config.LSPServerConfig{
				"go": {Addr: addr},
				// Port 1 is reserved: a connection is refused immediately.
				"java": {Addr: "127.0.0.1:1"},
			},
		},
	}

	probes := dependencyProbes(cfg)
	byName := map[string]probe{}
	for _, p := range probes {
		byName[p.name] = p
	}

	if p, ok := byName["storage.qdrant.url"]; !ok || p.err != nil {
		t.Errorf("qdrant probe = %+v, want a successful probe", p)
	}
	if p, ok := byName["lsp.servers.go"]; !ok || p.err != nil {
		t.Errorf("go lsp probe = %+v, want a successful probe", p)
	}
	if p, ok := byName["lsp.servers.java"]; !ok || p.err == nil {
		t.Errorf("java lsp probe = %+v, want a failed probe", p)
	}
}

func TestDependencyProbes_SkipsDisabledComponents(t *testing.T) {
	cfg := &config.Config{
		Storage: config.StorageConfig{SQLite: &config.SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
		Search:  &config.SearchConfig{Rerank: &config.RerankConfig{Enabled: false, BaseURL: "http://127.0.0.1:1"}},
		LSP:     &config.LSPConfig{Enabled: false, Servers: map[string]config.LSPServerConfig{"go": {Addr: "127.0.0.1:1"}}},
	}

	if got := dependencyProbes(cfg); len(got) != 0 {
		t.Errorf("probes = %+v, want none for disabled components", got)
	}
}

func TestRunCheckConfig_ExitCodes(t *testing.T) {
	valid := &config.Config{
		Storage: config.StorageConfig{SQLite: &config.SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1}},
	}
	if code := runCheckConfig(valid, writeConfigFile(t, valid)); code != 0 {
		t.Errorf("exit code = %d, want 0 for a valid config with no dependencies", code)
	}

	invalid := &config.Config{
		Storage: config.StorageConfig{SQLite: &config.SQLiteStorageConfig{Path: "", PoolSize: 0}},
	}
	if code := runCheckConfig(invalid, writeConfigFile(t, invalid)); code != exitConfigInvalid {
		t.Errorf("exit code = %d, want %d for an invalid config", code, exitConfigInvalid)
	}

	unreachable := &config.Config{
		Storage: config.StorageConfig{
			SQLite: &config.SQLiteStorageConfig{Path: "/tmp/t.db", PoolSize: 1},
			Qdrant: &config.QdrantStorageConfig{URL: "http://127.0.0.1:1"},
		},
	}
	if code := runCheckConfig(unreachable, writeConfigFile(t, unreachable)); code != exitDepUnreachable {
		t.Errorf("exit code = %d, want %d for an unreachable dependency", code, exitDepUnreachable)
	}
}

func TestResolveConfigPath(t *testing.T) {
	t.Setenv("RAGOTA_CONFIG", "/etc/ragota/from-env.yaml")

	if got := resolveConfigPath("/flag.yaml"); got != "/flag.yaml" {
		t.Errorf("path = %q, want the flag value to win", got)
	}
	if got := resolveConfigPath(""); got != "/etc/ragota/from-env.yaml" {
		t.Errorf("path = %q, want RAGOTA_CONFIG", got)
	}

	t.Setenv("RAGOTA_CONFIG", "")
	if got := resolveConfigPath(""); got != config.DefaultConfigPath {
		t.Errorf("path = %q, want %q", got, config.DefaultConfigPath)
	}
}

func TestSecondsAndParseLevel(t *testing.T) {
	if seconds(0) != 0 {
		t.Error("0 must disable the timeout")
	}
	if got := seconds(15); got.Seconds() != 15 {
		t.Errorf("seconds(15) = %v", got)
	}
	if parseLevel("debug").Level() >= parseLevel("info").Level() {
		t.Error("debug must be more verbose than info")
	}
	if parseLevel("nonsense") != parseLevel("info") {
		t.Error("an unknown level must fall back to info")
	}
}

func TestCheckConfig_LoadsTheExampleConfig(t *testing.T) {
	// config.example.yaml is the documented starting point: it must at least
	// load and validate after the placeholders are stripped.
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(config.example.yaml) error = %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.example.yaml does not validate: %v", err)
	}
	// The documented example must also pass the strict key check --check-config
	// runs, or the tool would reject its own example.
	if err := config.CheckUnknownKeys(path); err != nil {
		t.Fatalf("config.example.yaml has unknown keys: %v", err)
	}
}

func TestCheckConfig_LoadsTheDockerConfig(t *testing.T) {
	// deploy/config.docker.yaml is what the compose stack mounts; it must stay
	// loadable and valid as the schema evolves.
	t.Setenv("POSTGRES_PASSWORD", "secret")

	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "config.docker.yaml"))
	if err != nil {
		t.Fatalf("read docker config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(deploy/config.docker.yaml) error = %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("deploy/config.docker.yaml does not validate: %v", err)
	}
	if cfg.Storage.Postgres == nil || !strings.Contains(cfg.Storage.Postgres.DSN, "secret") {
		t.Errorf("DSN = %q, want the expanded password", cfg.Storage.Postgres.DSN)
	}
	if err := config.CheckUnknownKeys(path); err != nil {
		t.Fatalf("deploy/config.docker.yaml has unknown keys: %v", err)
	}
}

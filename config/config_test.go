package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigUsesConfigPathAndEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.toml")
	data := []byte(`
[server]
host = "127.0.0.1"
port = 5999

[access]
proxy = "socks5://127.0.0.1:1080"
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", path)
	t.Setenv("SERVER_PORT", "6001")
	t.Setenv("ACCESS_PROXY", "")

	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	cfg := GetConfig()
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("Server.Host = %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 6001 {
		t.Fatalf("Server.Port = %d, want 6001", cfg.Server.Port)
	}
	if cfg.Access.Proxy != "" {
		t.Fatalf("Access.Proxy = %q, want empty override", cfg.Access.Proxy)
	}
}

func TestValidateConfigFile(t *testing.T) {
	validPath := filepath.Join(t.TempDir(), "valid.toml")
	if err := os.WriteFile(validPath, []byte(`
[server]
port = 5000
`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ValidateConfigFile(validPath); err != nil {
		t.Fatalf("ValidateConfigFile(valid) error = %v", err)
	}

	invalidPath := filepath.Join(t.TempDir(), "invalid.toml")
	if err := os.WriteFile(invalidPath, []byte(`[server`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ValidateConfigFile(invalidPath); err == nil {
		t.Fatal("ValidateConfigFile(invalid) = nil, want error")
	}
}

func TestLoadConfigIgnoresLegacyRegistryEndpointFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-registry.toml")
	if err := os.WriteFile(path, []byte(`
[registries."ghcr.io"]
enabled = false
upstream = "https://example.invalid"
authHost = "https://example.invalid/token"
authType = "legacy"
`), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", path)
	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	cfg := GetConfig()
	if cfg.Registries["ghcr.io"].Enabled {
		t.Fatal(`Registries["ghcr.io"].Enabled = true, want false`)
	}
}

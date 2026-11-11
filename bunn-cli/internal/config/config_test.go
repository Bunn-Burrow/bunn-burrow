package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDefaultsApplied verifies that default values are used when no config
// file or environment variables are present.
func TestDefaultsApplied(t *testing.T) {
	loader := NewLoader()
	cfg, err := loader.Load("")
	require.NoError(t, err)

	require.Equal(t, "firecracker", cfg.Runtime.FirecrackerBin)
	require.Equal(t, 10*time.Second, cfg.Runtime.StartTimeout)
	require.Equal(t, 15*time.Second, cfg.Runtime.StopTimeout)
	require.Equal(t, "info", cfg.Logging.Level)
	require.Equal(t, "text", cfg.Logging.Format)
	require.Equal(t, "table", cfg.Output.Format)
	require.Equal(t, "auto", cfg.Output.Color)
	require.NotEmpty(t, cfg.Storage.RootDir)
	require.NotEmpty(t, cfg.Storage.CacheDir)
}

// TestConfigFileOverridesDefaults verifies that config file values override
// defaults.
func TestConfigFileOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	content := []byte(`runtime:
  firecracker_bin: /custom/firecracker
  start_timeout: 30s
logging:
  level: debug
output:
  format: json
`)
	require.NoError(t, os.WriteFile(configPath, content, 0o600))

	loader := NewLoader()
	cfg, err := loader.Load(configPath)
	require.NoError(t, err)

	require.Equal(t, "/custom/firecracker", cfg.Runtime.FirecrackerBin)
	require.Equal(t, 30*time.Second, cfg.Runtime.StartTimeout)
	require.Equal(t, 15*time.Second, cfg.Runtime.StopTimeout) // default preserved
	require.Equal(t, "debug", cfg.Logging.Level)
	require.Equal(t, "text", cfg.Logging.Format) // default preserved
	require.Equal(t, "json", cfg.Output.Format)
	require.Equal(t, "auto", cfg.Output.Color) // default preserved
}

// TestConfigFileMissingIsNotAnError verifies that a missing config file does
// not cause an error; defaults are used instead.
func TestConfigFileMissingIsNotAnError(t *testing.T) {
	loader := NewLoader()
	cfg, err := loader.Load("/nonexistent/path/config.yaml")
	require.NoError(t, err)
	require.Equal(t, "firecracker", cfg.Runtime.FirecrackerBin)
}

// TestEnvOverridesConfigFile verifies that environment variables override
// config file values.
func TestEnvOverridesConfigFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	content := []byte(`runtime:
  firecracker_bin: /config/firecracker
logging:
  level: info
`)
	require.NoError(t, os.WriteFile(configPath, content, 0o600))

	// Set environment variable to override config file
	t.Setenv("BUNN_BURROW_RUNTIME_FIRECRACKER_BIN", "/env/firecracker")
	t.Setenv("BUNN_BURROW_LOGGING_LEVEL", "trace")

	loader := NewLoader()
	cfg, err := loader.Load(configPath)
	require.NoError(t, err)

	require.Equal(t, "/env/firecracker", cfg.Runtime.FirecrackerBin)
	require.Equal(t, "trace", cfg.Logging.Level)
	require.Equal(t, "trace", cfg.Logging.Level) // env overrides config
}

// TestConfigPath validates ConfigPath resolution.
func TestConfigPath(t *testing.T) {
	// Default path
	defaultPath := ConfigPath("")
	require.Equal(t, expandHome(DefaultConfigPath), defaultPath)

	// Override path
	override := ConfigPath("~/custom/config.yaml")
	require.Equal(t, expandHome("~/custom/config.yaml"), override)
}

// TestSaveAndLoadRoundTrip verifies that saving and loading config preserves
// values.
func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "roundtrip.yaml")

	cfg := Defaults()
	cfg.Runtime.FirecrackerBin = "/saved/firecracker"
	cfg.Logging.Level = "debug"

	require.NoError(t, Save(cfg, configPath, true))

	loader := NewLoader()
	loaded, err := loader.Load(configPath)
	require.NoError(t, err)

	require.Equal(t, "/saved/firecracker", loaded.Runtime.FirecrackerBin)
	require.Equal(t, "debug", loaded.Logging.Level)
	require.Equal(t, Defaults().Runtime.StopTimeout, loaded.Runtime.StopTimeout)
}

// TestSaveRefusesToOverwrite verifies that Save refuses to overwrite an
// existing file unless force is true.
func TestSaveRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "noclobber.yaml")

	cfg := Defaults()
	require.NoError(t, Save(cfg, configPath, true))

	// Second save without force should fail
	err := Save(cfg, configPath, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")

	// Save with force should succeed
	require.NoError(t, Save(cfg, configPath, true))
}

// TestRedactedIsPassthrough documents that Redacted is a passthrough because
// no sensitive fields exist in the current config.
func TestRedactedIsPassthrough(t *testing.T) {
	cfg := Defaults()
	rc := Redacted(cfg)

	require.Equal(t, cfg.Runtime.FirecrackerBin, rc.Runtime.FirecrackerBin)
	require.Equal(t, cfg.Logging.Level, rc.Logging.Level)
	require.Equal(t, cfg.Output.Format, rc.Output.Format)
}

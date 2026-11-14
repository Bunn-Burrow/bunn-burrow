// Package config provides configuration loading and defaults for the bunn CLI.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

// DefaultConfigPath is the default location for the CLI configuration file.
const DefaultConfigPath = "~/.config/bunn-burrow/config.yaml"

// EnvPrefix is the environment variable prefix for configuration.
const EnvPrefix = "BUNN_BURROW"

// Config holds the Bunn Burrow CLI configuration.
type Config struct {
	Runtime RuntimeConfig `yaml:"runtime" mapstructure:"runtime"`
	Storage StorageConfig `yaml:"storage" mapstructure:"storage"`
	Logging LoggingConfig `yaml:"logging" mapstructure:"logging"`
	Output  OutputConfig  `yaml:"output" mapstructure:"output"`
	Network NetworkConfig `yaml:"network" mapstructure:"network"`
}

// RuntimeConfig holds Firecracker runtime settings.
type RuntimeConfig struct {
	FirecrackerBin string        `yaml:"firecracker_bin" mapstructure:"firecracker_bin"`
	KernelPath     string        `yaml:"kernel_path" mapstructure:"kernel_path"`
	RootFSPath     string        `yaml:"rootfs_path" mapstructure:"rootfs_path"`
	WorkDir        string        `yaml:"work_dir" mapstructure:"work_dir"`
	StartTimeout   time.Duration `yaml:"start_timeout" mapstructure:"start_timeout"`
	StopTimeout    time.Duration `yaml:"stop_timeout" mapstructure:"stop_timeout"`
}

// NetworkConfig holds VM networking settings.
type NetworkConfig struct {
	Enabled      bool     `yaml:"enabled" mapstructure:"enabled"`
	Bridge       string   `yaml:"bridge" mapstructure:"bridge"`
	CIDR         string   `yaml:"cidr" mapstructure:"cidr"`
	Gateway      string   `yaml:"gateway" mapstructure:"gateway"`
	DNS          []string `yaml:"dns" mapstructure:"dns"`
	NATInterface string   `yaml:"nat_interface" mapstructure:"nat_interface"`
	Firewall     bool     `yaml:"firewall" mapstructure:"firewall"`
	MTU          int      `yaml:"mtu" mapstructure:"mtu"`
}

// StorageConfig holds storage settings.
type StorageConfig struct {
	RootDir  string `yaml:"root_dir" mapstructure:"root_dir"`
	CacheDir string `yaml:"cache_dir" mapstructure:"cache_dir"`
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level  string `yaml:"level" mapstructure:"level"`
	Format string `yaml:"format" mapstructure:"format"`
}

// OutputConfig holds CLI output settings.
type OutputConfig struct {
	Format string `yaml:"format" mapstructure:"format"`
	Color  string `yaml:"color" mapstructure:"color"`
}

// Viper key constants for flag binding.
const (
	KeyRuntimeFirecrackerBin = "runtime.firecracker_bin"
	KeyRuntimeKernelPath     = "runtime.kernel_path"
	KeyRuntimeRootFSPath     = "runtime.rootfs_path"
	KeyRuntimeWorkDir        = "runtime.work_dir"
	KeyRuntimeStartTimeout   = "runtime.start_timeout"
	KeyRuntimeStopTimeout    = "runtime.stop_timeout"
	KeyStorageRootDir        = "storage.root_dir"
	KeyStorageCacheDir       = "storage.cache_dir"
	KeyLoggingLevel          = "logging.level"
	KeyLoggingFormat         = "logging.format"
	KeyOutputFormat          = "output.format"
	KeyOutputColor           = "output.color"
	KeyNetworkEnabled        = "network.enabled"
	KeyNetworkBridge         = "network.bridge"
	KeyNetworkCIDR           = "network.cidr"
	KeyNetworkGateway        = "network.gateway"
	KeyNetworkDNS            = "network.dns"
	KeyNetworkNATInterface   = "network.nat_interface"
	KeyNetworkFirewall       = "network.firewall"
	KeyNetworkMTU            = "network.mtu"
)

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Runtime: RuntimeConfig{
			FirecrackerBin: "firecracker",
			StartTimeout:   10 * time.Second,
			StopTimeout:    15 * time.Second,
		},
		Storage: StorageConfig{
			RootDir:  filepath.Join(home, ".local", "share", "bunn-burrow", "storage"),
			CacheDir: filepath.Join(home, ".cache", "bunn-burrow"),
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
		Output: OutputConfig{
			Format: "table",
			Color:  "auto",
		},
		Network: NetworkConfig{
			Enabled:  false,
			Bridge:   "br-bunn",
			CIDR:     "10.11.0.0/16",
			Firewall: true,
			MTU:      1500,
		},
	}
}

// Loader loads configuration from file, environment, and flags.
// Precedence: CLI flags > environment variables > config file > defaults.
type Loader struct {
	viper *viper.Viper
}

// NewLoader creates a new configuration loader with defaults and environment
// variable support.
func NewLoader() *Loader {
	v := viper.New()
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Set defaults explicitly.
	v.SetDefault(KeyRuntimeFirecrackerBin, Defaults().Runtime.FirecrackerBin)
	v.SetDefault(KeyRuntimeStartTimeout, Defaults().Runtime.StartTimeout)
	v.SetDefault(KeyRuntimeStopTimeout, Defaults().Runtime.StopTimeout)
	v.SetDefault(KeyStorageRootDir, Defaults().Storage.RootDir)
	v.SetDefault(KeyStorageCacheDir, Defaults().Storage.CacheDir)
	v.SetDefault(KeyLoggingLevel, Defaults().Logging.Level)
	v.SetDefault(KeyLoggingFormat, Defaults().Logging.Format)
	v.SetDefault(KeyOutputFormat, Defaults().Output.Format)
	v.SetDefault(KeyOutputColor, Defaults().Output.Color)
	v.SetDefault(KeyNetworkEnabled, Defaults().Network.Enabled)
	v.SetDefault(KeyNetworkBridge, Defaults().Network.Bridge)
	v.SetDefault(KeyNetworkCIDR, Defaults().Network.CIDR)
	v.SetDefault(KeyNetworkFirewall, Defaults().Network.Firewall)
	v.SetDefault(KeyNetworkMTU, Defaults().Network.MTU)

	return &Loader{viper: v}
}

// Load reads configuration from the given path, environment variables, and
// applies defaults. If path is empty, the default config path is used.
//
// Precedence: CLI flags (bound via BindPFlag) > environment variables >
// config file > defaults.
func (l *Loader) Load(configPath string) (Config, error) {
	cfg := Defaults()

	if configPath == "" {
		configPath = DefaultConfigPath
	}
	configPath = expandHome(configPath)

	if _, err := os.Stat(configPath); err == nil {
		l.viper.SetConfigFile(configPath)
		if err := l.viper.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("read config file %q: %w", configPath, err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("stat config file %q: %w", configPath, err)
	}

	// Unmarshal Viper's merged view (defaults + config file + env vars +
	// any bound CLI flags) into the config struct.
	if err := l.viper.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}

	return cfg, nil
}

// BindPFlag binds a Cobra/Pflag flag to a Viper key so that the flag value takes
// precedence over environment variables, config file values, and defaults.
// Call this after defining a flag on a command.
func (l *Loader) BindPFlag(key string, flag *pflag.Flag) {
	l.viper.BindPFlag(key, flag)
}

// Viper returns the underlying Viper instance for advanced use cases.
func (l *Loader) Viper() *viper.Viper {
	return l.viper
}

// Save writes the configuration to the given path. It refuses to overwrite an
// existing file unless force is true.
func Save(cfg Config, path string, force bool) error {
	path = expandHome(path)

	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("config file %q already exists; use --force to overwrite", path)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat config file %q: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create config directory %q: %w", dir, err)
		}
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config file %q: %w", path, err)
	}

	return nil
}

// ConfigPath returns the resolved configuration file path.
func ConfigPath(override string) string {
	if override != "" {
		return expandHome(override)
	}
	return expandHome(DefaultConfigPath)
}

// expandHome expands a leading ~ to the user's home directory.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return p
}

// Redacted returns a copy of the configuration safe for display.
// No sensitive fields (tokens, passwords, keys) exist in the current config,
// so this is a passthrough. It is retained for future-proofing.
func Redacted(cfg Config) Config {
	return cfg
}

// String returns a human-readable summary of the configuration.
func String(cfg Config) string {
	var b strings.Builder
	b.WriteString("Runtime:\n")
	b.WriteString(fmt.Sprintf("  firecracker_bin: %s\n", cfg.Runtime.FirecrackerBin))
	b.WriteString(fmt.Sprintf("  kernel_path: %s\n", cfg.Runtime.KernelPath))
	b.WriteString(fmt.Sprintf("  rootfs_path: %s\n", cfg.Runtime.RootFSPath))
	b.WriteString(fmt.Sprintf("  work_dir: %s\n", cfg.Runtime.WorkDir))
	b.WriteString(fmt.Sprintf("  start_timeout: %s\n", cfg.Runtime.StartTimeout))
	b.WriteString(fmt.Sprintf("  stop_timeout: %s\n", cfg.Runtime.StopTimeout))
	b.WriteString("Storage:\n")
	b.WriteString(fmt.Sprintf("  root_dir: %s\n", cfg.Storage.RootDir))
	b.WriteString(fmt.Sprintf("  cache_dir: %s\n", cfg.Storage.CacheDir))
	b.WriteString("Logging:\n")
	b.WriteString(fmt.Sprintf("  level: %s\n", cfg.Logging.Level))
	b.WriteString(fmt.Sprintf("  format: %s\n", cfg.Logging.Format))
	b.WriteString("Output:\n")
	b.WriteString(fmt.Sprintf("  format: %s\n", cfg.Output.Format))
	b.WriteString(fmt.Sprintf("  color: %s\n", cfg.Output.Color))
	b.WriteString("Network:\n")
	b.WriteString(fmt.Sprintf("  enabled: %v\n", cfg.Network.Enabled))
	b.WriteString(fmt.Sprintf("  bridge: %s\n", cfg.Network.Bridge))
	b.WriteString(fmt.Sprintf("  cidr: %s\n", cfg.Network.CIDR))
	b.WriteString(fmt.Sprintf("  gateway: %s\n", cfg.Network.Gateway))
	b.WriteString(fmt.Sprintf("  dns: %s\n", strings.Join(cfg.Network.DNS, ",")))
	b.WriteString(fmt.Sprintf("  nat_interface: %s\n", cfg.Network.NATInterface))
	b.WriteString(fmt.Sprintf("  firewall: %v\n", cfg.Network.Firewall))
	b.WriteString(fmt.Sprintf("  mtu: %d\n", cfg.Network.MTU))
	return b.String()
}

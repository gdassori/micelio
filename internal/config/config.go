package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration wraps time.Duration for TOML string unmarshaling (e.g. "30s", "5m").
// A zero-value Duration means "not configured" — callers should apply their own default.
// This is set automatically by Defaults(); it only stays zero when constructing
// configs manually (e.g. in tests) without calling Defaults().
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

type Config struct {
	Node    NodeConfig    `toml:"node"`
	SSH     SSHConfig     `toml:"ssh"`
	Network NetworkConfig `toml:"network"`
	Logging LoggingConfig `toml:"logging"`
}

type LoggingConfig struct {
	Level  string `toml:"level"`  // debug, info, warn, error (default: info)
	Format string `toml:"format"` // text or json (default: text)
}

type NodeConfig struct {
	Name    string   `toml:"name"`
	Tags    []string `toml:"tags"`
	DataDir string   `toml:"data_dir"`
}

type SSHConfig struct {
	Listen string `toml:"listen"`
}

type NetworkConfig struct {
	Listen            string   `toml:"listen"`
	AdvertiseAddr     string   `toml:"advertise_addr"` // explicit host:port to advertise; overrides Listen
	Bootstrap         []string `toml:"bootstrap"`
	MaxPeers          int      `toml:"max_peers"`
	ExchangeInterval  Duration `toml:"exchange_interval"`  // peer exchange period; 0 → 30s default
	DiscoveryInterval Duration `toml:"discovery_interval"` // discovery scan period; 0 → 10s default
}

// Defaults returns a Config with sane defaults.
func Defaults() *Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "micelio"
	}
	return &Config{
		Node: NodeConfig{
			Name:    hostname,
			DataDir: "~/.micelio",
		},
		SSH: SSHConfig{
			Listen: "0.0.0.0:2222",
		},
		Network: NetworkConfig{
			MaxPeers:          15,
			ExchangeInterval:  Duration{30 * time.Second},
			DiscoveryInterval: Duration{10 * time.Second},
		},
	}
}

// Load reads a TOML config file and returns the parsed Config.
// If path is empty, only defaults are returned.
func Load(path string) (*Config, error) {
	cfg := Defaults()

	if path == "" {
		// Try default location
		path = expandHome("~/.micelio/config.toml")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return cfg, nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return cfg, nil
}

// ExpandHome resolves a leading ~/ to the user's home directory.
func ExpandHome(path string) string {
	return expandHome(path)
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

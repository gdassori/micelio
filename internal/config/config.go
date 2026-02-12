package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Node    NodeConfig    `toml:"node"`
	SSH     SSHConfig     `toml:"ssh"`
	Network NetworkConfig `toml:"network"`
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
	Listen    string   `toml:"listen"`
	Bootstrap []string `toml:"bootstrap"`
	MaxPeers  int      `toml:"max_peers"`
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
			MaxPeers: 15,
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

package config

import (
	"errors"
	"fmt"
	"net"
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
	State   StateConfig   `toml:"state"`
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

type StateConfig struct {
	TombstoneTTL Duration `toml:"tombstone_ttl"` // how long tombstones live before GC; 0 → 24h default
	GCInterval   Duration `toml:"gc_interval"`   // GC scan period; 0 → 1h default
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
		State: StateConfig{
			TombstoneTTL: Duration{24 * time.Hour},
			GCInterval:   Duration{1 * time.Hour},
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

// Validate checks configured addresses, numeric fields, and logging level for validity.
// Returns all validation errors as a joined error (not just the first).
func (c *Config) Validate() error {
	var errs []error
	errs = append(errs, c.validateAddresses()...)
	errs = append(errs, c.validateNumericFields()...)
	if err := validateLogLevel(c.Logging.Level); err != nil {
		errs = append(errs, fmt.Errorf("logging.level: %w", err))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// validateAddresses validates all network and SSH address fields.
func (c *Config) validateAddresses() []error {
	var errs []error
	if c.Network.Listen != "" {
		if err := validateListenAddr(c.Network.Listen); err != nil {
			errs = append(errs, fmt.Errorf("network.listen: %w", err))
		}
	}
	if c.SSH.Listen != "" {
		if err := validateListenAddr(c.SSH.Listen); err != nil {
			errs = append(errs, fmt.Errorf("ssh.listen: %w", err))
		}
	}
	for i, addr := range c.Network.Bootstrap {
		if err := validatePeerAddr(addr); err != nil {
			errs = append(errs, fmt.Errorf("network.bootstrap[%d]: %w", i, err))
		}
	}
	if c.Network.AdvertiseAddr != "" {
		if err := validatePeerAddr(c.Network.AdvertiseAddr); err != nil {
			errs = append(errs, fmt.Errorf("network.advertise_addr: %w", err))
		}
	}
	return errs
}

// validateNumericFields validates all numeric config fields.
func (c *Config) validateNumericFields() []error {
	var errs []error
	if c.Network.MaxPeers < 0 {
		errs = append(errs, fmt.Errorf("network.max_peers: must be >= 0, got %d", c.Network.MaxPeers))
	}
	if c.Network.ExchangeInterval.Duration < 0 {
		errs = append(errs, fmt.Errorf("network.exchange_interval: must be >= 0, got %s", c.Network.ExchangeInterval))
	}
	if c.Network.DiscoveryInterval.Duration < 0 {
		errs = append(errs, fmt.Errorf("network.discovery_interval: must be >= 0, got %s", c.Network.DiscoveryInterval))
	}
	return errs
}

// validateListenAddr validates a listen address (host:port).
// Accepts wildcard hosts (0.0.0.0, ::).
func validateListenAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fmt.Errorf("address is empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid format (expected host:port): %w", err)
	}
	if port == "" {
		return fmt.Errorf("missing port")
	}
	// Listen addresses can be wildcards, so we only reject empty host
	if host == "" {
		return fmt.Errorf("missing host")
	}
	return nil
}

// validatePeerAddr validates a peer address (host:port).
// Rejects wildcard/unspecified hosts as they're not dialable.
func validatePeerAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fmt.Errorf("address is empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid format (expected host:port): %w", err)
	}
	if port == "" {
		return fmt.Errorf("missing port")
	}
	if host == "" {
		return fmt.Errorf("wildcard host not allowed for peer address")
	}
	// Reject unspecified IP addresses (0.0.0.0, ::, and all variants)
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return fmt.Errorf("wildcard host not allowed for peer address")
	}
	return nil
}

// validateLogLevel validates a log level string.
// Accepts: debug, info, warn, warning, error (case-insensitive), and empty string.
// Empty string defaults to "info" in logging.Init().
func validateLogLevel(level string) error {
	normalized := strings.ToLower(strings.TrimSpace(level))
	if normalized == "" {
		return nil // empty is valid, defaults to info
	}
	switch normalized {
	case "debug", "info", "warn", "warning", "error":
		return nil
	default:
		return fmt.Errorf("invalid level %q (expected: debug, info, warn, warning, error)", level)
	}
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

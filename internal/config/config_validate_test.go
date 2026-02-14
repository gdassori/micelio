package config

import (
	"strings"
	"testing"
	"time"
)

const (
	errMissingPort       = "missing port"
	errExpectedError     = "expected error"
	errUnexpectedError   = "unexpected error: %v"
	errExpectedValErr    = "expected validation error"
	testAddrIPv4         = "0.0.0.0:8080"
	testAddrIPv6         = "[::]:9000"
	testAddrWildcardAlt  = "0.0.0.0:9000"
	testAddrWildcardHTTP = "0.0.0.0:80"
	testAddrMissingPort  = "missing-port"
	testAddrEmptyHost    = ":8080"
)

func TestConfigValidate_Valid(t *testing.T) {
	cfg := Defaults()
	cfg.Network.Listen = "0.0.0.0:5000"
	cfg.SSH.Listen = "127.0.0.1:2222"
	cfg.Network.Bootstrap = []string{"127.0.0.1:8080", "example.com:9000"}
	cfg.Logging.Level = "debug"

	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config should pass validation: %v", err)
	}
}

func TestConfigValidate_EmptyOptionalFields(t *testing.T) {
	// Empty Network.Listen, empty bootstrap, empty log level should all be valid
	cfg := Defaults()
	cfg.Network.Listen = ""
	cfg.Network.Bootstrap = []string{}
	cfg.Logging.Level = ""

	if err := cfg.Validate(); err != nil {
		t.Errorf("config with empty optional fields should be valid: %v", err)
	}
}

func TestConfigValidate_InvalidNetworkListen(t *testing.T) {
	tests := []struct {
		name   string
		listen string
	}{
		{errMissingPort, "127.0.0.1"},
		{"colon only", ":8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Network.Listen = tt.listen

			err := cfg.Validate()
			if err == nil {
				t.Fatal(errExpectedValErr)
			}
			if !strings.Contains(err.Error(), "network.listen") {
				t.Errorf("error should mention 'network.listen': %v", err)
			}
		})
	}
}

func TestConfigValidate_InvalidSSHListen(t *testing.T) {
	tests := []struct {
		name   string
		listen string
	}{
		{errMissingPort, "localhost"},
		{"colon only", ":"},
		{"empty host", ":2222"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.SSH.Listen = tt.listen

			err := cfg.Validate()
			if err == nil {
				t.Fatal(errExpectedValErr)
			}
			if !strings.Contains(err.Error(), "ssh.listen") {
				t.Errorf("error should mention 'ssh.listen': %v", err)
			}
		})
	}
}

func TestConfigValidate_InvalidBootstrap(t *testing.T) {
	tests := []struct {
		name      string
		bootstrap string
		wantIndex int
	}{
		{"wildcard IPv4", testAddrIPv4, 0},
		{"wildcard IPv6", testAddrIPv6, 0},
		{errMissingPort, "example.com", 0},
		{"empty", "", 0},
		{"second entry invalid", testAddrWildcardAlt, 1}, // first valid, second invalid
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			if tt.wantIndex == 1 {
				// Test second entry invalid (first is valid, second is wildcard)
				cfg.Network.Bootstrap = []string{"valid.example.com:8080", tt.bootstrap}
			} else {
				cfg.Network.Bootstrap = []string{tt.bootstrap}
			}

			err := cfg.Validate()
			if err == nil {
				t.Fatal(errExpectedValErr)
			}

			errStr := err.Error()
			wantSubstr := "network.bootstrap["
			if !strings.Contains(errStr, wantSubstr) {
				t.Errorf("error should mention 'network.bootstrap[': %v", errStr)
			}
		})
	}
}

func TestConfigValidate_InvalidMaxPeers(t *testing.T) {
	cfg := Defaults()
	cfg.Network.MaxPeers = -5

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for negative MaxPeers")
	}
	if !strings.Contains(err.Error(), "network.max_peers") {
		t.Errorf("error should mention 'network.max_peers': %v", err)
	}
	if !strings.Contains(err.Error(), "-5") {
		t.Errorf("error should include the invalid value: %v", err)
	}
}

func TestConfigValidate_InvalidIntervals(t *testing.T) {
	tests := []struct {
		name      string
		exchange  time.Duration
		discovery time.Duration
		wantErr   string
	}{
		{
			name:      "negative exchange interval",
			exchange:  -5 * time.Second,
			discovery: 10 * time.Second,
			wantErr:   "network.exchange_interval",
		},
		{
			name:      "negative discovery interval",
			exchange:  30 * time.Second,
			discovery: -10 * time.Second,
			wantErr:   "network.discovery_interval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Network.ExchangeInterval = Duration{tt.exchange}
			cfg.Network.DiscoveryInterval = Duration{tt.discovery}

			err := cfg.Validate()
			if err == nil {
				t.Fatal(errExpectedValErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error should mention %q: %v", tt.wantErr, err)
			}
		})
	}
}

func TestConfigValidate_InvalidLogLevel(t *testing.T) {
	tests := []struct {
		level    string
		wantErr  bool
	}{
		{"unknown", true},
		{"trace", true},
		{"fatal", true},
		{"INVALID", true},
		{"debug", false},
		{"info", false},
		{"warn", false},
		{"warning", false},
		{"error", false},
		{"DEBUG", false}, // case insensitive
		{"  Error  ", false}, // whitespace
		{"", false}, // empty defaults to info
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			cfg := Defaults()
			cfg.Logging.Level = tt.level

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal(errExpectedValErr)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for valid level: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "logging.level") {
				t.Errorf("error should mention 'logging.level': %v", err)
			}
		})
	}
}

func TestConfigValidate_MultipleErrors(t *testing.T) {
	cfg := &Config{
		Network: NetworkConfig{
			Listen:            "invalid",                   // missing port
			AdvertiseAddr:     testAddrIPv4,                // wildcard not allowed
			Bootstrap:         []string{testAddrWildcardHTTP}, // wildcard not allowed
			MaxPeers:          -5,                          // negative
			ExchangeInterval:  Duration{-5 * time.Second},  // negative
			DiscoveryInterval: Duration{10 * time.Second},
		},
		SSH: SSHConfig{
			Listen: testAddrMissingPort,
		},
		Logging: LoggingConfig{
			Level: "invalid-level",
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}

	errStr := err.Error()
	expectedErrors := []string{
		"network.listen",
		"network.advertise_addr",
		"network.bootstrap[0]",
		"network.max_peers",
		"network.exchange_interval",
		"ssh.listen",
		"logging.level",
	}

	for _, expected := range expectedErrors {
		if !strings.Contains(errStr, expected) {
			t.Errorf("error missing %q: %v", expected, errStr)
		}
	}
}

func TestValidateListenAddr(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{testAddrIPv4, false},
		{"127.0.0.1:5000", false},
		{testAddrIPv6, false},
		{"localhost:8080", false},
		{"  127.0.0.1:8080  ", false}, // whitespace trimmed
		{"no-port", true},
		{"", true},
		{"   ", true},        // whitespace-only
		{":8080", true},      // empty host
		{"host:", true},      // empty port
		{" :8080 ", true},    // whitespace around empty host
		{" host: ", true},    // whitespace around empty port
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			err := validateListenAddr(tt.addr)
			if tt.wantErr && err == nil {
				t.Error(errExpectedError)
			}
			if !tt.wantErr && err != nil {
				t.Errorf(errUnexpectedError, err)
			}
		})
	}
}

func TestValidatePeerAddr(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{"127.0.0.1:8080", false},
		{"example.com:5000", false},
		{"[2001:db8::1]:9000", false},
		{"  example.com:8080  ", false},      // whitespace trimmed
		{testAddrIPv4, true},                 // wildcard not allowed
		{testAddrIPv6, true},                 // wildcard not allowed
		{"[0:0:0:0:0:0:0:0]:8080", true},     // IPv6 unspecified variant
		{"[0000:0000:0000:0000:0000:0000:0000:0000]:8080", true}, // IPv6 unspecified long form
		{"no-port", true},
		{"", true},
		{"   ", true},                        // whitespace-only
		{":8080", true},                      // empty host
		{"  :8080  ", true},                  // whitespace around empty host
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			err := validatePeerAddr(tt.addr)
			if tt.wantErr && err == nil {
				t.Error(errExpectedError)
			}
			if !tt.wantErr && err != nil {
				t.Errorf(errUnexpectedError, err)
			}
		})
	}
}

func TestValidateLogLevel(t *testing.T) {
	tests := []struct {
		level   string
		wantErr bool
	}{
		{"debug", false},
		{"info", false},
		{"warn", false},
		{"warning", false},
		{"error", false},
		{"DEBUG", false}, // case insensitive
		{"  Info  ", false}, // whitespace
		{"", false}, // empty is valid
		{"unknown", true},
		{"trace", true},
		{"fatal", true},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			err := validateLogLevel(tt.level)
			if tt.wantErr && err == nil {
				t.Error(errExpectedError)
			}
			if !tt.wantErr && err != nil {
				t.Errorf(errUnexpectedError, err)
			}
		})
	}
}

func TestConfigValidate_InvalidAdvertiseAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"wildcard IPv4", testAddrIPv4},
		{"wildcard IPv6", testAddrIPv6},
		{"missing port", "example.com"},
		{"empty host", testAddrEmptyHost},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Network.AdvertiseAddr = tt.addr

			err := cfg.Validate()
			if err == nil {
				t.Fatal(errExpectedValErr)
			}
			if !strings.Contains(err.Error(), "network.advertise_addr") {
				t.Errorf("error should mention 'network.advertise_addr': %v", err)
			}
		})
	}
}

func TestConfigValidate_AdvertiseAddrEmpty(t *testing.T) {
	cfg := Defaults()
	cfg.Network.AdvertiseAddr = ""

	err := cfg.Validate()
	if err != nil {
		t.Errorf("empty advertise_addr should be valid, got error: %v", err)
	}
}

func TestConfigValidate_IntervalsAllowZero(t *testing.T) {
	tests := []struct {
		name      string
		exchange  time.Duration
		discovery time.Duration
	}{
		{
			name:      "zero exchange interval",
			exchange:  0,
			discovery: 10 * time.Second,
		},
		{
			name:      "zero discovery interval",
			exchange:  30 * time.Second,
			discovery: 0,
		},
		{
			name:      "both zero",
			exchange:  0,
			discovery: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Network.ExchangeInterval = Duration{tt.exchange}
			cfg.Network.DiscoveryInterval = Duration{tt.discovery}

			err := cfg.Validate()
			if err != nil {
				t.Errorf("zero intervals should be valid (mean use default), got error: %v", err)
			}
		})
	}
}

func TestValidateLogLevel_WhitespaceOnly(t *testing.T) {
	tests := []string{
		"   ",
		"\t",
		"\n",
		"  \t\n  ",
	}

	for _, level := range tests {
		t.Run("whitespace", func(t *testing.T) {
			err := validateLogLevel(level)
			if err != nil {
				t.Errorf("whitespace-only level should be valid (defaults to info), got error: %v", err)
			}
		})
	}
}

package transport

import (
	"testing"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/partyline"
)

func TestAdvertiseAddr(t *testing.T) {
	dir := t.TempDir()
	id, err := identity.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub := partyline.NewHub("test")

	tests := []struct {
		name          string
		advertiseAddr string
		listenAddr    string // simulated bound address
		want          string
	}{
		{"explicit_advertise_addr", "1.2.3.4:9000", "0.0.0.0:9000", "1.2.3.4:9000"},
		{"non_wildcard_listen", "", "127.0.0.1:9000", "127.0.0.1:9000"},
		{"wildcard_0000", "", "0.0.0.0:9000", ""},
		{"wildcard_ipv6", "", "[::]:9000", ""},
		{"no_listen", "", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Node: config.NodeConfig{Name: "test"},
				Network: config.NetworkConfig{
					AdvertiseAddr: tc.advertiseAddr,
				},
			}
			mgr, err := NewManager(cfg, id, hub, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Simulate the bound listen address
			mgr.mu.Lock()
			mgr.listenAddr = tc.listenAddr
			mgr.mu.Unlock()

			got := mgr.advertiseAddr()
			if got != tc.want {
				t.Errorf("advertiseAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

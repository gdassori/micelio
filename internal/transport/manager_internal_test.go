package transport

import (
	"context"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/logging"
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

func TestStartFailureCleansUpGossip(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	dir := t.TempDir()
	id, err := identity.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub := partyline.NewHub("test")

	cfg := &config.Config{
		Node:    config.NodeConfig{Name: "test"},
		Network: config.NetworkConfig{Listen: "999.999.999.999:0"},
	}
	mgr, err := NewManager(cfg, id, hub, nil)
	if err != nil {
		t.Fatal(err)
	}

	go hub.Run()
	defer hub.Stop()

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = mgr.Start(ctx)
	if err == nil {
		mgr.Stop()
		t.Fatal("expected Start to fail with invalid listen address")
	}

	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	if after > before+1 {
		t.Errorf("goroutine leak: before=%d after=%d (gossip not cleaned up?)", before, after)
	}
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", capture.Count(slog.LevelError))
	}
}

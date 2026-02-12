package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.SSH.Listen != "0.0.0.0:2222" {
		t.Errorf("SSH listen: got %q, want 0.0.0.0:2222", cfg.SSH.Listen)
	}
	if cfg.Node.DataDir != "~/.micelio" {
		t.Errorf("DataDir: got %q, want ~/.micelio", cfg.Node.DataDir)
	}
	if cfg.Network.MaxPeers != 15 {
		t.Errorf("MaxPeers: got %d, want 15", cfg.Network.MaxPeers)
	}
	if cfg.Node.Name == "" {
		t.Error("Node.Name should default to hostname")
	}
}

func TestLoadNoFile(t *testing.T) {
	// Load with empty path and no default config file → returns defaults
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSH.Listen != "0.0.0.0:2222" {
		t.Errorf("SSH listen: got %q, want 0.0.0.0:2222", cfg.SSH.Listen)
	}
}

func TestLoadTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[node]
name = "test-node"
tags = ["test", "ci"]
data_dir = "/tmp/micelio-test"

[ssh]
listen = "127.0.0.1:3333"

[network]
listen = "0.0.0.0:5000"
bootstrap = ["peer1:4000", "peer2:4000"]
max_peers = 10
`
	if err := os.WriteFile(path, []byte(toml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Node.Name != "test-node" {
		t.Errorf("Node.Name: got %q, want test-node", cfg.Node.Name)
	}
	if len(cfg.Node.Tags) != 2 || cfg.Node.Tags[0] != "test" {
		t.Errorf("Node.Tags: got %v", cfg.Node.Tags)
	}
	if cfg.SSH.Listen != "127.0.0.1:3333" {
		t.Errorf("SSH.Listen: got %q", cfg.SSH.Listen)
	}
	if cfg.Network.Listen != "0.0.0.0:5000" {
		t.Errorf("Network.Listen: got %q", cfg.Network.Listen)
	}
	if len(cfg.Network.Bootstrap) != 2 {
		t.Errorf("Bootstrap: got %v", cfg.Network.Bootstrap)
	}
	if cfg.Network.MaxPeers != 10 {
		t.Errorf("MaxPeers: got %d", cfg.Network.MaxPeers)
	}
}

func TestLoadBadTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte("{{invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	got := ExpandHome("~/foo/bar")
	want := filepath.Join(home, "foo/bar")
	if got != want {
		t.Errorf("ExpandHome: got %q, want %q", got, want)
	}

	// Non-home path unchanged
	if got := ExpandHome("/absolute/path"); got != "/absolute/path" {
		t.Errorf("ExpandHome: got %q, want /absolute/path", got)
	}
}

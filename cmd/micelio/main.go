package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/partyline"
	"micelio/internal/ssh"
	boltstore "micelio/internal/store/bolt"
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	dataDir := flag.String("data-dir", "", "data directory (overrides config)")
	sshListen := flag.String("ssh-listen", "", "SSH listen address (overrides config)")
	nodeName := flag.String("name", "", "node name (overrides config)")
	flag.Parse()

	// Load config (TOML file with defaults)
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// CLI flags override config file values
	if *dataDir != "" {
		cfg.Node.DataDir = *dataDir
	}
	if *sshListen != "" {
		cfg.SSH.Listen = *sshListen
	}
	if *nodeName != "" {
		cfg.Node.Name = *nodeName
	}

	cfg.Node.DataDir = config.ExpandHome(cfg.Node.DataDir)

	if err := os.MkdirAll(cfg.Node.DataDir, 0700); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}

	// Load or generate ED25519 identity
	id, err := identity.Load(cfg.Node.DataDir)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	log.Printf("Node ID:   %s", id.NodeID)
	log.Printf("Node name: %s", cfg.Node.Name)

	// Open persistent store
	dbPath := filepath.Join(cfg.Node.DataDir, "data.db")
	store, err := boltstore.Open(dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	// Start partyline hub
	hub := partyline.NewHub(cfg.Node.Name)
	go hub.Run()

	// Start SSH server
	authKeysPath := filepath.Join(cfg.Node.DataDir, "authorized_keys")
	sshServer, err := ssh.NewServer(cfg.SSH.Listen, id, hub, authKeysPath)
	if err != nil {
		log.Fatalf("ssh: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := sshServer.Start(ctx); err != nil {
			log.Fatalf("ssh: %v", err)
		}
	}()

	log.Printf("SSH partyline listening on %s", cfg.SSH.Listen)

	// Graceful shutdown on SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	cancel()
	sshServer.Stop()
	hub.Stop()
}

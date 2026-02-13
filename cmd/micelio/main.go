package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/logging"
	"micelio/internal/partyline"
	"micelio/internal/ssh"
	boltstore "micelio/internal/store/bolt"
	"micelio/internal/transport"
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	dataDir := flag.String("data-dir", "", "data directory (overrides config)")
	sshListen := flag.String("ssh-listen", "", "SSH listen address (overrides config)")
	nodeName := flag.String("name", "", "node name (overrides config)")
	logLevel := flag.String("log-level", "", "log level: debug, info, warn, error (overrides config)")
	flag.Parse()

	// Load config (TOML file with defaults)
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("fatal: config", "err", err)
		os.Exit(1)
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

	// Resolve log level: CLI flag > config > env > default (info)
	level := cfg.Logging.Level
	if *logLevel != "" {
		level = *logLevel
	}
	if level == "" {
		level = os.Getenv("MICELIO_LOG_LEVEL")
	}
	logging.Init(level, cfg.Logging.Format)

	cfg.Node.DataDir = config.ExpandHome(cfg.Node.DataDir)

	if err := os.MkdirAll(cfg.Node.DataDir, 0700); err != nil {
		slog.Error("fatal: creating data dir", "err", err)
		os.Exit(1)
	}

	// Load or generate ED25519 identity
	id, err := identity.Load(cfg.Node.DataDir)
	if err != nil {
		slog.Error("fatal: identity", "err", err)
		os.Exit(1)
	}
	slog.Info("node started", "node_id", id.NodeID, "name", cfg.Node.Name)

	// Open persistent store
	dbPath := filepath.Join(cfg.Node.DataDir, "data.db")
	store, err := boltstore.Open(dbPath)
	if err != nil {
		slog.Error("fatal: store", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	// Start partyline hub
	hub := partyline.NewHub(cfg.Node.Name)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create transport manager before hub.Run() to avoid data race on remoteSend.
	// Manager is created when listen OR bootstrap is configured (supports outbound-only nodes).
	var mgr *transport.Manager
	if cfg.Network.Listen != "" || len(cfg.Network.Bootstrap) > 0 {
		var err2 error
		mgr, err2 = transport.NewManager(cfg, id, hub, store)
		if err2 != nil {
			slog.Error("fatal: transport", "err", err2)
			os.Exit(1)
		}
	}

	go hub.Run()

	if mgr != nil {
		go func() {
			if err := mgr.Start(ctx); err != nil {
				slog.Error("transport failed", "err", err)
			}
		}()
		if cfg.Network.Listen != "" {
			slog.Info("transport listening", "addr", cfg.Network.Listen)
		}
		if len(cfg.Network.Bootstrap) > 0 {
			slog.Info("bootstrapping", "peers", len(cfg.Network.Bootstrap))
		}
	}

	// Start SSH server
	authKeysPath := filepath.Join(cfg.Node.DataDir, "authorized_keys")
	sshServer, err := ssh.NewServer(cfg.SSH.Listen, id, hub, authKeysPath)
	if err != nil {
		slog.Error("fatal: ssh", "err", err)
		os.Exit(1)
	}

	go func() {
		if err := sshServer.Start(ctx); err != nil {
			slog.Error("fatal: ssh", "err", err)
			os.Exit(1)
		}
	}()

	slog.Info("ssh listening", "addr", cfg.SSH.Listen)

	// Graceful shutdown on SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutting down")
	cancel()
	if mgr != nil {
		mgr.Stop()
	}
	sshServer.Stop()
	hub.Stop()
}

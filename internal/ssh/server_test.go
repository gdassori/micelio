package ssh_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"micelio/internal/identity"
	"micelio/internal/logging"
	"micelio/internal/partyline"
	sshserver "micelio/internal/ssh"

	gossh "golang.org/x/crypto/ssh"
)

func TestSSHPartyline(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	tmpDir := t.TempDir()

	// Server identity
	id, err := identity.Load(tmpDir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	// Client key → authorized_keys
	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating client key: %v", err)
	}
	sshPub, err := gossh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatalf("converting client key: %v", err)
	}
	authKeysPath := filepath.Join(tmpDir, "authorized_keys")
	if err := os.WriteFile(authKeysPath, gossh.MarshalAuthorizedKey(sshPub), 0600); err != nil {
		t.Fatalf("writing authorized_keys: %v", err)
	}

	// Hub
	hub := partyline.NewHub("test-node")
	go hub.Run()
	defer hub.Stop()

	// Server on random port
	srv, err := sshserver.NewServer("127.0.0.1:0", id, hub, authKeysPath)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		srv.Stop()
	}()
	go func() { _ = srv.Serve(ctx) }()

	// SSH client
	clientSigner, err := gossh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	client, err := gossh.Dial("tcp", srv.Addr(), &gossh.ClientConfig{
		User:            "tester",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(clientSigner)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer func() { _ = session.Close() }()

	if err := session.RequestPty("xterm", 40, 80, gossh.TerminalModes{}); err != nil {
		t.Fatalf("pty: %v", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := session.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	// Accumulate output in background
	var mu sync.Mutex
	var buf strings.Builder
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := stdout.Read(tmp)
			if n > 0 {
				mu.Lock()
				buf.Write(tmp[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// pos tracks where we last matched, so each waitFor only looks at new output
	pos := 0

	waitFor := func(substr string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := buf.String()
			mu.Unlock()
			if idx := strings.Index(got[pos:], substr); idx >= 0 {
				pos += idx + len(substr)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		t.Fatalf("timeout waiting for %q in output:\n%s", substr, got[pos:])
	}

	send := func(cmd string) {
		if _, err := stdin.Write([]byte(cmd + "\r")); err != nil {
			t.Fatalf("writing command %q: %v", cmd, err)
		}
	}

	// Welcome
	waitFor("Welcome to test-node partyline!")

	// /who — should list "tester"
	send("/who")
	waitFor("Online (1): tester")

	// /nick — change nick and verify
	send("/nick hacker")
	waitFor("now known as hacker")

	// /who again — should show new nick
	send("/who")
	waitFor("Online (1): hacker")

	// /help
	send("/help")
	waitFor("Commands:")
	waitFor("/who")
	waitFor("/nick <name>")
	waitFor("/quit")
	waitFor("/help")

	// /nick without args
	send("/nick")
	waitFor("Usage: /nick <name>")

	// unknown command
	send("/bogus")
	waitFor("Unknown command: /bogus")

	// /quit
	send("/quit")
	waitFor("Goodbye")

	time.Sleep(100 * time.Millisecond) // let server process disconnect

	// Log assertions
	if !capture.Has(slog.LevelInfo, "client connected") {
		t.Error("expected INFO log: client connected")
	}
	if !capture.Has(slog.LevelDebug, "session joined") {
		t.Error("expected DEBUG log: session joined")
	}
	if !capture.Has(slog.LevelDebug, "session left") {
		t.Error("expected DEBUG log: session left")
	}
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", capture.Count(slog.LevelError))
	}
}

func TestSSHServerStart(t *testing.T) {
	tmpDir := t.TempDir()

	id, err := identity.Load(tmpDir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	clientPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating client key: %v", err)
	}
	sshPub, err := gossh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatalf("converting client key: %v", err)
	}
	authKeysPath := filepath.Join(tmpDir, "authorized_keys")
	if err := os.WriteFile(authKeysPath, gossh.MarshalAuthorizedKey(sshPub), 0600); err != nil {
		t.Fatalf("writing authorized_keys: %v", err)
	}

	hub := partyline.NewHub("start-test")
	go hub.Run()
	defer hub.Stop()

	srv, err := sshserver.NewServer("127.0.0.1:0", id, hub, authKeysPath)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	// Wait for the server to be listening
	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("server did not start listening")
	}

	cancel()
	srv.Stop()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancel+stop")
	}
}

func TestCommandRegistryFreezesAfterListen(t *testing.T) {
	tmpDir := t.TempDir()

	id, err := identity.Load(tmpDir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	hub := partyline.NewHub("freeze-test")
	go hub.Run()
	defer hub.Stop()

	srv, err := sshserver.NewServer("127.0.0.1:0", id, hub, filepath.Join(tmpDir, "authorized_keys"))
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}

	// Should be able to register before Listen
	srv.Commands().Register("/custom", sshserver.Command{
		Help: "custom command",
		Handler: func(_ sshserver.CommandContext) bool { return false },
	})

	// Listen freezes the registry
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srv.Stop()

	// Attempting to register after Listen should panic
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when registering after Listen")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "frozen") {
			t.Errorf("unexpected panic value: %v", r)
		}
	}()
	srv.Commands().Register("/toobad", sshserver.Command{
		Help: "should panic",
		Handler: func(_ sshserver.CommandContext) bool { return false },
	})
}

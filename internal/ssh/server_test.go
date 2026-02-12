package ssh_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"micelio/internal/identity"
	"micelio/internal/partyline"
	sshserver "micelio/internal/ssh"

	gossh "golang.org/x/crypto/ssh"
)

func TestSSHPartyline(t *testing.T) {
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
	go srv.Serve(ctx)

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
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

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

	// /quit
	send("/quit")
	waitFor("Goodbye")
}

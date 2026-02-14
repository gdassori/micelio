package ssh_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
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

// testServer bundles a running SSH server with a client signer for tests.
type testServer struct {
	Srv          *sshserver.Server
	Hub          *partyline.Hub
	ClientSigner gossh.Signer
}

// newTestServer creates an identity, client key, authorized_keys, hub, and SSH
// server ready for use. The hub is started and cleanup is registered via t.Cleanup.
func newTestServer(t *testing.T, hubName string) *testServer {
	t.Helper()

	tmpDir := t.TempDir()
	id, err := identity.Load(tmpDir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	sshPub, err := gossh.NewPublicKey(clientPriv.Public())
	if err != nil {
		t.Fatalf("ssh public key: %v", err)
	}
	authKeysPath := filepath.Join(tmpDir, "authorized_keys")
	if err := os.WriteFile(authKeysPath, gossh.MarshalAuthorizedKey(sshPub), 0600); err != nil {
		t.Fatalf("authorized_keys: %v", err)
	}
	clientSigner, err := gossh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}

	hub := partyline.NewHub(hubName)
	go hub.Run()
	t.Cleanup(hub.Stop)

	srv, err := sshserver.NewServer("127.0.0.1:0", id, hub, authKeysPath)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}

	return &testServer{Srv: srv, Hub: hub, ClientSigner: clientSigner}
}

// dialClient connects an SSH client to the server and returns the client.
func (ts *testServer) dialClient(t *testing.T, user string) *gossh.Client {
	t.Helper()
	client, err := gossh.Dial("tcp", ts.Srv.Addr(), &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(ts.ClientSigner)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	return client
}

// openShell opens a pty+shell session and returns the session, stdin, and stdout.
func (ts *testServer) openShell(t *testing.T, client *gossh.Client) (*gossh.Session, io.WriteCloser, io.Reader) {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
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
	return session, stdin, stdout
}

// outputAccumulator reads from r in the background and provides waitFor.
type outputAccumulator struct {
	mu  sync.Mutex
	buf strings.Builder
	pos int
}

func newOutputAccumulator(r io.Reader) *outputAccumulator {
	a := &outputAccumulator{}
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				a.mu.Lock()
				a.buf.Write(tmp[:n])
				a.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return a
}

func (a *outputAccumulator) waitFor(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		got := a.buf.String()
		a.mu.Unlock()
		if idx := strings.Index(got[a.pos:], substr); idx >= 0 {
			a.pos += idx + len(substr)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.mu.Lock()
	got := a.buf.String()
	a.mu.Unlock()
	t.Fatalf("timeout waiting for %q in output:\n%s", substr, got[a.pos:])
}

func TestSSHPartyline(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	ts := newTestServer(t, "test-node")
	if err := ts.Srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		ts.Srv.Stop()
	}()
	go func() { _ = ts.Srv.Serve(ctx) }()

	client := ts.dialClient(t, "tester")
	defer func() { _ = client.Close() }()

	session, stdin, stdout := ts.openShell(t, client)
	defer func() { _ = session.Close() }()

	out := newOutputAccumulator(stdout)

	send := func(cmd string) {
		if _, err := stdin.Write([]byte(cmd + "\r")); err != nil {
			t.Fatalf("writing command %q: %v", cmd, err)
		}
	}

	// Welcome
	out.waitFor(t, "Welcome to test-node partyline!")

	// /who — should list "tester"
	send("/who")
	out.waitFor(t, "Online (1): tester")

	// /nick — change nick and verify
	send("/nick hacker")
	out.waitFor(t, "now known as hacker")

	// /who again — should show new nick
	send("/who")
	out.waitFor(t, "Online (1): hacker")

	// /help
	send("/help")
	out.waitFor(t, "Commands:")
	out.waitFor(t, "/who")
	out.waitFor(t, "/nick <name>")
	out.waitFor(t, "/quit")
	out.waitFor(t, "/help")

	// /nick without args
	send("/nick")
	out.waitFor(t, "Usage: /nick <name>")

	// unknown command
	send("/bogus")
	out.waitFor(t, "Unknown command: /bogus")

	// /quit
	send("/quit")
	out.waitFor(t, "Goodbye")

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
	ts := newTestServer(t, "start-test")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- ts.Srv.Start(ctx)
	}()

	// Wait for the server to be listening
	deadline := time.Now().Add(5 * time.Second)
	for ts.Srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ts.Srv.Addr() == "" {
		t.Fatal("server did not start listening")
	}

	cancel()
	ts.Srv.Stop()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancel+stop")
	}
}

func TestSSHStopDrainsConnections(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	ts := newTestServer(t, "drain-test")
	if err := ts.Srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = ts.Srv.Serve(ctx) }()

	client := ts.dialClient(t, "drainer")
	_, _, stdoutR := ts.openShell(t, client)
	out := newOutputAccumulator(stdoutR)
	out.waitFor(t, "Welcome")

	// Shut down: cancel context + Stop()
	cancel()

	stopDone := make(chan struct{})
	go func() {
		ts.Srv.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		// Good: Stop() completed, meaning it waited for connections to drain
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not return within timeout")
	}

	// Log assertions
	if !capture.Has(slog.LevelInfo, "client connected") {
		t.Error("expected INFO log: client connected")
	}
	if capture.Has(slog.LevelWarn, "timeout waiting for SSH connections to drain") {
		t.Error("Stop() timed out instead of draining cleanly")
	}
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", capture.Count(slog.LevelError))
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

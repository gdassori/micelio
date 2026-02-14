package transport_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/logging"
	"micelio/internal/partyline"
	"micelio/internal/transport"
)

// testPair holds two connected managers for keepalive/timeout tests.
type testPair struct {
	MgrA, MgrB       *transport.Manager
	CancelA, CancelB context.CancelFunc
	Capture          *logging.Capture
}

// setupTestPair creates two nodes, connects A→B, and waits for the connection.
// Each side gets its own idle timeout and keepalive interval.
// Cleanup is registered via t.Cleanup — callers don't need defer.
func setupTestPair(t *testing.T, idleA, kaA, idleB, kaB time.Duration) *testPair {
	t.Helper()

	capture := logging.CaptureForTest()

	dirA, dirB := t.TempDir(), t.TempDir()
	idA, err := identity.Load(dirA)
	if err != nil {
		t.Fatalf("identity A: %v", err)
	}
	idB, err := identity.Load(dirB)
	if err != nil {
		t.Fatalf("identity B: %v", err)
	}

	hubA := partyline.NewHub("node-a")
	hubB := partyline.NewHub("node-b")
	go hubA.Run()
	go hubB.Run()

	cfgB := &config.Config{
		Node:    config.NodeConfig{Name: "node-b", DataDir: dirB},
		Network: config.NetworkConfig{Listen: "127.0.0.1:0"},
	}
	mgrB, err := transport.NewManager(cfgB, idB, hubB, nil)
	if err != nil {
		t.Fatalf("manager B: %v", err)
	}
	mgrB.SetPeerTimeouts(idleB, kaB)

	ctxB, cancelB := context.WithCancel(context.Background())
	go func() { _ = mgrB.Start(ctxB) }()

	deadline := time.Now().Add(5 * time.Second)
	for mgrB.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if mgrB.Addr() == "" {
		cancelB()
		t.Fatal("manager B failed to start listener")
	}

	cfgA := &config.Config{
		Node: config.NodeConfig{Name: "node-a", DataDir: dirA},
		Network: config.NetworkConfig{
			Listen:    "127.0.0.1:0",
			Bootstrap: []string{mgrB.Addr()},
		},
	}
	mgrA, err := transport.NewManager(cfgA, idA, hubA, nil)
	if err != nil {
		cancelB()
		t.Fatalf("manager A: %v", err)
	}
	mgrA.SetPeerTimeouts(idleA, kaA)

	ctxA, cancelA := context.WithCancel(context.Background())
	go func() { _ = mgrA.Start(ctxA) }()

	// Wait for connection.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgrA.PeerCount() > 0 && mgrB.PeerCount() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgrA.PeerCount() == 0 || mgrB.PeerCount() == 0 {
		cancelA()
		cancelB()
		t.Fatalf("peers not connected: A=%d B=%d", mgrA.PeerCount(), mgrB.PeerCount())
	}

	tp := &testPair{
		MgrA: mgrA, MgrB: mgrB,
		CancelA: cancelA, CancelB: cancelB,
		Capture: capture,
	}
	t.Cleanup(func() {
		cancelA()
		cancelB()
		mgrA.Stop()
		mgrB.Stop()
		hubA.Stop()
		hubB.Stop()
		capture.Restore()
	})

	return tp
}

// TestKeepalivePreventTimeout verifies that idle connections stay alive when
// keepalives are active. Two nodes are connected with short timeouts. No chat
// messages are exchanged. After waiting well beyond the idle timeout, both
// nodes should still be connected because keepalives reset the read deadline.
func TestKeepalivePreventTimeout(t *testing.T) {
	tp := setupTestPair(t,
		2*time.Second, 500*time.Millisecond, // A: 2s idle, 500ms keepalive
		2*time.Second, 500*time.Millisecond, // B: 2s idle, 500ms keepalive
	)

	// Wait 3x the idle timeout — keepalives should keep the connection alive.
	// PeerExchange (default interval 30s) cannot interfere here because the
	// idle timeout is only 2s, so keepalives (500ms) are the sole mechanism
	// resetting the read deadline.
	time.Sleep(6 * time.Second)

	if tp.MgrA.PeerCount() == 0 {
		t.Error("node A lost connection despite keepalives")
	}
	if tp.MgrB.PeerCount() == 0 {
		t.Error("node B lost connection despite keepalives")
	}

	if !tp.Capture.Has(slog.LevelInfo, "peer connected") {
		t.Error("expected INFO log: peer connected")
	}
	// No idle timeout warnings should appear — keepalives prevent them.
	if tp.Capture.Has(slog.LevelWarn, "peer idle timeout") {
		t.Error("unexpected WARN: peer idle timeout (keepalives should prevent this)")
	}
	if tp.Capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", tp.Capture.Count(slog.LevelError))
	}
}

// TestIdleTimeout verifies that a silent peer is disconnected after the idle
// timeout. Node A connects to node B with a very long keepalive interval (1h),
// effectively disabling keepalives. Node B should disconnect A because no data
// arrives from A within the idle timeout.
func TestIdleTimeout(t *testing.T) {
	tp := setupTestPair(t,
		2*time.Second, 1*time.Hour, // A: 2s idle, no keepalive (1h)
		2*time.Second, 500*time.Millisecond, // B: 2s idle, 500ms keepalive
	)

	// Cancel A's context so dialWithBackoff stops reconnecting after B
	// disconnects A. Without this, A would reconnect immediately and reset
	// B's read deadline with a fresh PeerHello, preventing the idle timeout.
	tp.CancelA()

	// Wait for B to detect A's silence and disconnect.
	// B has idleTimeout=2s and A sends no keepalives.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if tp.MgrB.PeerCount() == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if tp.MgrB.PeerCount() != 0 {
		t.Fatal("node B should have disconnected silent peer A")
	}

	if !tp.Capture.Has(slog.LevelInfo, "peer connected") {
		t.Error("expected INFO log: peer connected")
	}
	if !tp.Capture.Has(slog.LevelWarn, "peer idle timeout") {
		t.Error("expected WARN log: peer idle timeout")
	}
	if !tp.Capture.Has(slog.LevelInfo, "peer disconnected") {
		t.Error("expected INFO log: peer disconnected")
	}
}

// TestWriteTimeout verifies that a peer is disconnected when writes time out.
// After connecting normally, node A's write timeout is set to 1ns, making any
// subsequent write deadline expire before the write completes. The next
// keepalive write should fail with a timeout, triggering disconnection.
func TestWriteTimeout(t *testing.T) {
	tp := setupTestPair(t,
		10*time.Second, 200*time.Millisecond, // A: long idle, fast keepalive
		10*time.Second, 200*time.Millisecond, // B: long idle, fast keepalive
	)

	// After connection is established, set A's write timeout to 1ns.
	// The next keepalive write will hit the deadline immediately.
	tp.MgrA.SetPeerWriteTimeout(time.Nanosecond)

	// Wait for A to fail on the next write.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if tp.MgrA.PeerCount() == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if tp.MgrA.PeerCount() != 0 {
		t.Fatal("node A should have disconnected after write timeout")
	}

	if !tp.Capture.Has(slog.LevelWarn, "peer write timeout") {
		t.Error("expected WARN log: peer write timeout")
	}
	if !tp.Capture.Has(slog.LevelInfo, "peer disconnected") {
		t.Error("expected INFO log: peer disconnected")
	}
}

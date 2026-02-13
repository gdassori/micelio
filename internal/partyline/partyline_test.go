package partyline

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"micelio/internal/logging"
)

func recv(ch <-chan string, timeout time.Duration) string {
	select {
	case msg := <-ch:
		return msg
	case <-time.After(timeout):
		return ""
	}
}

func drain(ch <-chan string, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ch:
		case <-timer.C:
			return
		}
	}
}

func TestJoinLeave(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	h := NewHub("test-node")
	go h.Run()
	defer h.Stop()

	s := h.Join("alice")
	if s.Nick != "alice" {
		t.Errorf("nick: got %q, want alice", s.Nick)
	}

	// Drain alice's own join notification
	drain(s.Send, 100*time.Millisecond)

	// Should receive join notification on a second session
	s2 := h.Join("bob")
	msg := recv(s.Send, time.Second)
	if !strings.Contains(msg, "bob joined") {
		t.Errorf("expected join notification, got %q", msg)
	}

	drain(s2.Send, 100*time.Millisecond)

	h.Leave(s2)
	msg = recv(s.Send, time.Second)
	if !strings.Contains(msg, "bob left") {
		t.Errorf("expected leave notification, got %q", msg)
	}

	h.Leave(s)
	time.Sleep(50 * time.Millisecond) // let hub process final leave

	if !capture.Has(slog.LevelDebug, "session joined") {
		t.Error("expected DEBUG log: session joined")
	}
	if !capture.Has(slog.LevelDebug, "session left") {
		t.Error("expected DEBUG log: session left")
	}
	if capture.Count(slog.LevelWarn) != 0 {
		t.Errorf("unexpected WARN logs: %d", capture.Count(slog.LevelWarn))
	}
}

func TestBroadcast(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	h := NewHub("test-node")
	go h.Run()
	defer h.Stop()

	alice := h.Join("alice")
	bob := h.Join("bob")
	drain(alice.Send, 100*time.Millisecond)
	drain(bob.Send, 100*time.Millisecond)

	h.Broadcast(alice, "hello world")

	// Bob should receive it
	msg := recv(bob.Send, time.Second)
	if !strings.Contains(msg, "alice") || !strings.Contains(msg, "hello world") {
		t.Errorf("bob got %q, want [alice] hello world", msg)
	}

	// Alice should NOT receive her own message
	msg = recv(alice.Send, 200*time.Millisecond)
	if msg != "" {
		t.Errorf("alice should not get own message, got %q", msg)
	}

	h.Leave(alice)
	h.Leave(bob)
	time.Sleep(50 * time.Millisecond)

	if !capture.Has(slog.LevelDebug, "session joined") {
		t.Error("expected DEBUG log: session joined")
	}
	if !capture.Has(slog.LevelDebug, "session left") {
		t.Error("expected DEBUG log: session left")
	}
	if capture.Count(slog.LevelWarn) != 0 {
		t.Errorf("unexpected WARN logs: %d", capture.Count(slog.LevelWarn))
	}
}

func TestWho(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	h := NewHub("test-node")
	go h.Run()
	defer h.Stop()

	alice := h.Join("alice")
	bob := h.Join("bob")
	drain(alice.Send, 100*time.Millisecond)
	drain(bob.Send, 100*time.Millisecond)

	nicks := h.Who()
	if len(nicks) != 2 {
		t.Fatalf("Who: got %d nicks, want 2", len(nicks))
	}

	found := map[string]bool{}
	for _, n := range nicks {
		found[n] = true
	}
	if !found["alice"] || !found["bob"] {
		t.Errorf("Who: got %v, want alice and bob", nicks)
	}

	h.Leave(alice)
	h.Leave(bob)
	time.Sleep(50 * time.Millisecond)

	if !capture.Has(slog.LevelDebug, "session joined") {
		t.Error("expected DEBUG log: session joined")
	}
	if !capture.Has(slog.LevelDebug, "session left") {
		t.Error("expected DEBUG log: session left")
	}
}

func TestSetNick(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	h := NewHub("test-node")
	go h.Run()
	defer h.Stop()

	alice := h.Join("alice")
	bob := h.Join("bob")
	drain(alice.Send, 100*time.Millisecond)
	drain(bob.Send, 100*time.Millisecond)

	old := h.SetNick(alice, "alicia")
	if old != "alice" {
		t.Errorf("SetNick returned %q, want alice", old)
	}

	msg := recv(bob.Send, time.Second)
	if !strings.Contains(msg, "alice") || !strings.Contains(msg, "alicia") {
		t.Errorf("nick change notification: got %q", msg)
	}

	h.Leave(alice)
	h.Leave(bob)
	time.Sleep(50 * time.Millisecond)

	if !capture.Has(slog.LevelDebug, "session joined") {
		t.Error("expected DEBUG log: session joined")
	}
	if !capture.Has(slog.LevelDebug, "session left") {
		t.Error("expected DEBUG log: session left")
	}
}

func TestDeliverRemote(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	h := NewHub("test-node")
	go h.Run()
	defer h.Stop()

	s := h.Join("local-user")
	drain(s.Send, 100*time.Millisecond)

	h.DeliverRemote("remote-nick", "hi from afar")

	msg := recv(s.Send, time.Second)
	if !strings.Contains(msg, "remote-nick") || !strings.Contains(msg, "hi from afar") {
		t.Errorf("DeliverRemote: got %q", msg)
	}

	h.Leave(s)
	time.Sleep(50 * time.Millisecond)

	if !capture.Has(slog.LevelDebug, "session joined") {
		t.Error("expected DEBUG log: session joined")
	}
	if !capture.Has(slog.LevelDebug, "session left") {
		t.Error("expected DEBUG log: session left")
	}
}

func TestRemoteSend(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	h := NewHub("test-node")
	remoteCh := make(chan RemoteMsg, 10)
	h.SetRemoteSend(remoteCh)
	go h.Run()
	defer h.Stop()

	alice := h.Join("alice")
	drain(alice.Send, 100*time.Millisecond)

	h.Broadcast(alice, "hello remote")

	select {
	case rm := <-remoteCh:
		if rm.Nick != "alice" || rm.Text != "hello remote" {
			t.Errorf("RemoteMsg: got %+v", rm)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for RemoteMsg")
	}

	// System messages should NOT be forwarded to remote
	h.DeliverRemote("remote", "echo test")
	select {
	case rm := <-remoteCh:
		t.Errorf("system message should not be forwarded, got %+v", rm)
	case <-time.After(200 * time.Millisecond):
		// expected
	}

	h.Leave(alice)
	time.Sleep(50 * time.Millisecond)

	if !capture.Has(slog.LevelDebug, "session joined") {
		t.Error("expected DEBUG log: session joined")
	}
	// No buffer-full warnings expected (channel has buffer of 10)
	if capture.Has(slog.LevelWarn, "remote send buffer full") {
		t.Error("unexpected WARN: remote send buffer full")
	}
}

func TestNodeName(t *testing.T) {
	h := NewHub("my-node")
	if h.NodeName() != "my-node" {
		t.Errorf("NodeName: got %q, want my-node", h.NodeName())
	}
}

func TestSystemMessage(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	h := NewHub("test-node")
	go h.Run()
	defer h.Stop()

	s := h.Join("user")
	drain(s.Send, 100*time.Millisecond)

	h.SystemMessage("system announcement")

	msg := recv(s.Send, time.Second)
	if msg != "system announcement" {
		t.Errorf("SystemMessage: got %q, want %q", msg, "system announcement")
	}

	h.Leave(s)
	time.Sleep(50 * time.Millisecond)

	if !capture.Has(slog.LevelDebug, "session joined") {
		t.Error("expected DEBUG log: session joined")
	}
	if !capture.Has(slog.LevelDebug, "session left") {
		t.Error("expected DEBUG log: session left")
	}
}

func TestRemoteSendBufferFull(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	h := NewHub("test-node")
	remoteCh := make(chan RemoteMsg) // unbuffered, no reader — always full
	h.SetRemoteSend(remoteCh)
	go h.Run()
	defer h.Stop()

	alice := h.Join("alice")
	drain(alice.Send, 100*time.Millisecond)

	h.Broadcast(alice, "will-drop")
	time.Sleep(100 * time.Millisecond) // let hub process the broadcast

	if !capture.Has(slog.LevelWarn, "remote send buffer full") {
		t.Error("expected WARN log: remote send buffer full")
	}

	h.Leave(alice)
}

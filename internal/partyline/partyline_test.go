package partyline

import (
	"strings"
	"testing"
	"time"
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
}

func TestBroadcast(t *testing.T) {
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
}

func TestWho(t *testing.T) {
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
}

func TestSetNick(t *testing.T) {
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
}

func TestDeliverRemote(t *testing.T) {
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
}

func TestRemoteSend(t *testing.T) {
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
}

func TestNodeName(t *testing.T) {
	h := NewHub("my-node")
	if h.NodeName() != "my-node" {
		t.Errorf("NodeName: got %q, want my-node", h.NodeName())
	}
}

func TestSystemMessage(t *testing.T) {
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
}

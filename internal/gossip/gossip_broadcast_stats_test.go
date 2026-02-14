package gossip

import (
	"log/slog"
	"sync"
	"testing"

	"micelio/internal/logging"
)

func TestBroadcastReturnsCorrectCounts(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	var sendCalls int
	var mu sync.Mutex

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool {
				mu.Lock()
				defer mu.Unlock()
				sendCalls++
				return true
			}},
			{NodeID: "peer-b", Send: func(data []byte) bool {
				mu.Lock()
				defer mu.Unlock()
				sendCalls++
				return false // simulate buffer full
			}},
			{NodeID: "peer-c", Send: func(data []byte) bool {
				mu.Lock()
				defer mu.Unlock()
				sendCalls++
				return true
			}},
		}
	})

	env := makeSignedEnvelope(t, local, 3, []byte("test"), 5)
	sent, total := engine.Broadcast(env)

	if sent != 2 {
		t.Errorf("sent: got %d, want 2", sent)
	}
	if total != 3 {
		t.Errorf("total: got %d, want 3", total)
	}

	mu.Lock()
	calls := sendCalls
	mu.Unlock()

	if calls != 3 {
		t.Errorf("Send() calls: got %d, want 3", calls)
	}

	// Verify WARN log
	if !capture.Has(slog.LevelWarn, "partial broadcast delivery") {
		t.Error("expected WARN log for partial delivery")
	}

	// Verify stats
	stats := engine.Stats()
	if stats.Broadcast != 2 {
		t.Errorf("broadcast: got %d, want 2 (successful sends)", stats.Broadcast)
	}
	if stats.Dropped != 1 {
		t.Errorf("dropped: got %d, want 1", stats.Dropped)
	}
}

func TestBroadcastPartialDeliveryWarning(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool { return true }},
			{NodeID: "peer-b", Send: func(data []byte) bool { return false }},
			{NodeID: "peer-c", Send: func(data []byte) bool { return false }},
			{NodeID: "peer-d", Send: func(data []byte) bool { return true }},
		}
	})

	env := makeSignedEnvelope(t, local, 3, []byte("test"), 5)
	sent, total := engine.Broadcast(env)

	if sent != 2 {
		t.Errorf("sent: got %d, want 2", sent)
	}
	if total != 4 {
		t.Errorf("total: got %d, want 4", total)
	}

	// Verify WARN log appears
	if !capture.Has(slog.LevelWarn, "partial broadcast delivery") {
		t.Error("expected WARN 'partial broadcast delivery'")
	}

	// Verify counters
	stats := engine.Stats()
	if stats.Broadcast != 2 {
		t.Errorf("broadcast: got %d, want 2 (successful sends)", stats.Broadcast)
	}
	if stats.Dropped != 2 {
		t.Errorf("dropped: got %d, want 2", stats.Dropped)
	}
}

func TestBroadcastAllSuccess(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool { return true }},
			{NodeID: "peer-b", Send: func(data []byte) bool { return true }},
		}
	})

	env := makeSignedEnvelope(t, local, 3, []byte("test"), 5)
	sent, total := engine.Broadcast(env)

	if sent != 2 {
		t.Errorf("sent: got %d, want 2", sent)
	}
	if total != 2 {
		t.Errorf("total: got %d, want 2", total)
	}

	// No WARN log when all succeed
	if capture.Count(slog.LevelWarn) != 0 {
		t.Errorf("unexpected WARN logs: %d", capture.Count(slog.LevelWarn))
	}

	// Verify counters
	stats := engine.Stats()
	if stats.Broadcast != 2 {
		t.Errorf("broadcast: got %d, want 2 (all successful)", stats.Broadcast)
	}
	if stats.Dropped != 0 {
		t.Errorf("dropped: got %d, want 0", stats.Dropped)
	}
}

func TestForwardToLogsDebugCounts(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var forwardCount int
	var mu sync.Mutex

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool {
				mu.Lock()
				defer mu.Unlock()
				forwardCount++
				return true
			}},
			{NodeID: "peer-b", Send: func(data []byte) bool {
				mu.Lock()
				defer mu.Unlock()
				forwardCount++
				return false // buffer full
			}},
			{NodeID: "peer-c", Send: func(data []byte) bool {
				mu.Lock()
				defer mu.Unlock()
				forwardCount++
				return true
			}},
		}
	})

	// Add sender to keyring
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)

	// Send message that will be forwarded (hop count > 1)
	// Exclude peer-a, so only peer-b and peer-c are candidates
	env := makeSignedEnvelope(t, sender, 3, []byte("forward me"), 5)
	engine.HandleIncoming("peer-a", env)

	// Verify DEBUG log appears
	if !capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("expected DEBUG 'message forwarded'")
	}

	// Verify stats
	// peer-a excluded, peer-b (fails) and peer-c (succeeds) are candidates
	// So forwarded should be 1, dropped should be 1
	stats := engine.Stats()
	if stats.Forwarded != 1 {
		t.Errorf("forwarded: got %d, want 1 (peer-c succeeded)", stats.Forwarded)
	}
	if stats.Dropped != 1 {
		t.Errorf("dropped: got %d, want 1 (peer-b failed)", stats.Dropped)
	}

	mu.Lock()
	calls := forwardCount
	mu.Unlock()

	if calls != 2 {
		t.Errorf("Send() calls: got %d, want 2 (peer-b and peer-c)", calls)
	}
}

func TestStats(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var deliveredCount int
	var mu sync.Mutex

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool { return true }},
			{NodeID: "peer-b", Send: func(data []byte) bool { return false }},
		}
	})

	// Register handler to track delivery
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		mu.Lock()
		deliveredCount++
		mu.Unlock()
	})

	// Add sender to keyring
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)

	// Receive and deliver message
	env1 := makeSignedEnvelope(t, sender, 3, []byte("message 1"), 5)
	engine.HandleIncoming("peer-x", env1)

	// Receive message without local delivery (unknown type)
	env2 := makeSignedEnvelope(t, sender, 99, []byte("message 2"), 5)
	engine.HandleIncoming("peer-y", env2)

	// Broadcast message
	env3 := makeSignedEnvelope(t, local, 3, []byte("broadcast"), 5)
	engine.Broadcast(env3)

	// Verify stats
	stats := engine.Stats()

	if stats.Received != 2 {
		t.Errorf("received: got %d, want 2", stats.Received)
	}
	if stats.Delivered != 1 {
		t.Errorf("delivered: got %d, want 1 (only msg type 3 should be delivered)", stats.Delivered)
	}
	if stats.Broadcast != 1 {
		t.Errorf("broadcast: got %d, want 1 (peer-a succeeded)", stats.Broadcast)
	}
	if stats.Forwarded != 2 {
		t.Errorf("forwarded: got %d, want 2 (2 messages forwarded to fanout subset)", stats.Forwarded)
	}
	if stats.Dropped != 3 {
		t.Errorf("dropped: got %d, want 3 (1 from broadcast + 2 from forwarding)", stats.Dropped)
	}

	// Verify actual delivery happened
	mu.Lock()
	delivered := deliveredCount
	mu.Unlock()

	if delivered != 1 {
		t.Errorf("handler called: got %d, want 1", delivered)
	}
}

func TestStatsAtomicity(t *testing.T) {
	// Verify Stats() can be called concurrently without panics
	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool { return true }},
		}
	})

	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	engine.RegisterHandler(3, func(senderID string, payload []byte) {})

	var wg sync.WaitGroup
	wg.Add(100)

	// Concurrent message handling
	for i := 0; i < 50; i++ {
		go func() {
			defer wg.Done()
			env := makeSignedEnvelope(t, sender, 3, []byte("test"), 5)
			engine.HandleIncoming("peer-x", env)
		}()
	}

	// Concurrent Stats() reads
	for i := 0; i < 50; i++ {
		go func() {
			defer wg.Done()
			_ = engine.Stats()
		}()
	}

	wg.Wait()

	// Just verify we didn't panic
	stats := engine.Stats()
	if stats.Received == 0 {
		t.Error("expected some received messages")
	}
}

func TestBroadcastNoPeers(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	// Engine with no peers
	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{}
	})

	env := makeSignedEnvelope(t, local, 3, []byte("test"), 5)
	sent, total := engine.Broadcast(env)

	if sent != 0 {
		t.Errorf("sent: got %d, want 0", sent)
	}
	if total != 0 {
		t.Errorf("total: got %d, want 0", total)
	}

	// No WARN log when there are no peers
	if capture.Count(slog.LevelWarn) != 0 {
		t.Errorf("unexpected WARN logs: %d", capture.Count(slog.LevelWarn))
	}

	// No counters incremented
	stats := engine.Stats()
	if stats.Broadcast != 0 {
		t.Errorf("broadcast: got %d, want 0", stats.Broadcast)
	}
	if stats.Dropped != 0 {
		t.Errorf("dropped: got %d, want 0", stats.Dropped)
	}
}

func TestForwardToZeroCandidates(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var sendCalls int
	var mu sync.Mutex

	// Only one peer - will be excluded as the sender
	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool {
				mu.Lock()
				defer mu.Unlock()
				sendCalls++
				return true
			}},
		}
	})

	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)

	// Message from peer-a should not be forwarded back to peer-a
	env := makeSignedEnvelope(t, sender, 3, []byte("no forward"), 5)
	engine.HandleIncoming("peer-a", env)

	// No Send() calls should happen
	mu.Lock()
	calls := sendCalls
	mu.Unlock()

	if calls != 0 {
		t.Errorf("Send() calls: got %d, want 0 (sender excluded)", calls)
	}

	// No forwarding stats
	stats := engine.Stats()
	if stats.Forwarded != 0 {
		t.Errorf("forwarded: got %d, want 0", stats.Forwarded)
	}
	if stats.Dropped != 0 {
		t.Errorf("dropped: got %d, want 0 (no candidates)", stats.Dropped)
	}

	// Message was received and processed
	if stats.Received != 1 {
		t.Errorf("received: got %d, want 1", stats.Received)
	}
}

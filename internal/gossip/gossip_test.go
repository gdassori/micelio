package gossip

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"micelio/internal/logging"
	pb "micelio/pkg/proto"
)

type testIdentity struct {
	nodeID string
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
}

func newTestIdentity(t *testing.T) testIdentity {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return testIdentity{
		nodeID: "test-node-" + uuid.New().String()[:8],
		pub:    pub,
		priv:   priv,
	}
}

func makeSignedEnvelope(t *testing.T, id testIdentity, msgType uint32, payload []byte, hopCount uint32) *pb.Envelope {
	t.Helper()
	env := &pb.Envelope{
		MessageId: uuid.New().String(),
		SenderId:  id.nodeID,
		MsgType:   msgType,
		HopCount:  hopCount,
		Payload:   payload,
	}
	pb.SignEnvelope(env, id.priv)
	return env
}

func TestEngineDedup(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var delivered int
	var mu sync.Mutex

	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		mu.Lock()
		delivered++
		mu.Unlock()
	})

	env := makeSignedEnvelope(t, sender, 3, []byte("hello"), 5)

	// First delivery
	engine.HandleIncoming("peer-a", env)

	// Duplicate delivery (same message ID)
	engine.HandleIncoming("peer-b", env)

	mu.Lock()
	defer mu.Unlock()
	if delivered != 1 {
		t.Fatalf("expected 1 delivery, got %d", delivered)
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG 'message received'")
	}
	if !capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("expected DEBUG 'message delivered locally'")
	}
	if capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("unexpected 'message forwarded' with no peers")
	}
}

func TestEngineUnknownSender(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var delivered int

	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
	// NOT adding sender to keyring
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	env := makeSignedEnvelope(t, sender, 3, []byte("hello"), 5)
	engine.HandleIncoming("peer-a", env)

	if delivered != 0 {
		t.Fatal("expected no delivery for unknown sender")
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG 'message received'")
	}
	if !capture.Has(slog.LevelWarn, "dropping message from unknown sender") {
		t.Error("expected WARN 'dropping message from unknown sender'")
	}
	if capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("unexpected 'message delivered locally' for unknown sender")
	}
}

func TestEngineAutoLearnKey(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	// Create a sender with a real nodeID = sha256(pubkey)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(pub)
	realNodeID := hex.EncodeToString(hash[:])

	var delivered int
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
	// NOT adding sender to keyring — should auto-learn from sender_pubkey
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	env := &pb.Envelope{
		MessageId:    uuid.New().String(),
		SenderId:     realNodeID,
		MsgType:      3,
		HopCount:     5,
		Payload:      []byte("hello"),
		SenderPubkey: pub,
	}
	pb.SignEnvelope(env, priv)

	engine.HandleIncoming("peer-a", env)

	if delivered != 1 {
		t.Fatalf("expected 1 delivery via auto-learn, got %d", delivered)
	}

	// Verify the key was added to the keyring
	entry, ok := engine.KeyRing.Lookup(realNodeID)
	if !ok {
		t.Fatal("expected key to be auto-learned into keyring")
	}
	if entry.TrustLevel != TrustGossipLearned {
		t.Fatalf("expected TrustGossipLearned, got %d", entry.TrustLevel)
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "auto-learned key") {
		t.Error("expected DEBUG 'auto-learned key'")
	}
	if !capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("expected DEBUG 'message delivered locally'")
	}
}

func TestEngineAutoLearnKeyBadHash(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var delivered int
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	// sender_id does NOT match sha256(sender_pubkey)
	env := &pb.Envelope{
		MessageId:    uuid.New().String(),
		SenderId:     "fake-node-id",
		MsgType:      3,
		HopCount:     5,
		Payload:      []byte("hello"),
		SenderPubkey: pub,
	}
	pb.SignEnvelope(env, priv)

	engine.HandleIncoming("peer-a", env)

	if delivered != 0 {
		t.Fatal("expected no delivery when sender_pubkey hash doesn't match sender_id")
	}

	// Log assertions
	if !capture.Has(slog.LevelWarn, "dropping message from unknown sender") {
		t.Error("expected WARN 'dropping message from unknown sender'")
	}
}

func TestEngineInvalidSignature(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)
	impostor := newTestIdentity(t)

	var delivered int

	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
	// Add sender's key, but the message is signed with impostor's key
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	env := &pb.Envelope{
		MessageId: uuid.New().String(),
		SenderId:  sender.nodeID, // claims to be sender
		MsgType:   3,
		HopCount:  5,
		Payload:   []byte("hello"),
	}
	pb.SignEnvelope(env, impostor.priv) // but signed by impostor

	engine.HandleIncoming("peer-a", env)

	if delivered != 0 {
		t.Fatal("expected no delivery for invalid signature")
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG 'message received'")
	}
	if !capture.Has(slog.LevelWarn, "invalid signature") {
		t.Error("expected WARN 'invalid signature'")
	}
	if capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("unexpected 'message delivered locally' with invalid signature")
	}
}

func TestEngineHopCountZero(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var forwarded []string
	var mu sync.Mutex

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{{
			NodeID: "peer-b",
			Send: func(data []byte) bool {
				mu.Lock()
				forwarded = append(forwarded, "peer-b")
				mu.Unlock()
				return true
			},
		}}
	})
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)

	var delivered int
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	// hop_count=1: should deliver locally but NOT forward
	env := makeSignedEnvelope(t, sender, 3, []byte("hello"), 1)
	engine.HandleIncoming("peer-a", env)

	if delivered != 1 {
		t.Fatalf("expected 1 delivery, got %d", delivered)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(forwarded) != 0 {
		t.Fatalf("expected no forwarding with hop_count=1, got %d", len(forwarded))
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG 'message received'")
	}
	if !capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("expected DEBUG 'message delivered locally'")
	}
	if capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("unexpected 'message forwarded' with hop_count=1")
	}
}

func TestEngineForwarding(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var forwarded []string
	var mu sync.Mutex

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool {
				mu.Lock()
				forwarded = append(forwarded, "peer-a")
				mu.Unlock()
				return true
			}},
			{NodeID: "peer-b", Send: func(data []byte) bool {
				mu.Lock()
				forwarded = append(forwarded, "peer-b")
				mu.Unlock()
				return true
			}},
			{NodeID: "peer-c", Send: func(data []byte) bool {
				mu.Lock()
				forwarded = append(forwarded, "peer-c")
				mu.Unlock()
				return true
			}},
		}
	})
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	engine.RegisterHandler(3, func(senderID string, payload []byte) {})

	env := makeSignedEnvelope(t, sender, 3, []byte("hello"), 5)
	engine.HandleIncoming("peer-a", env) // received from peer-a

	mu.Lock()
	defer mu.Unlock()

	// Should forward to peer-b and/or peer-c, but NOT peer-a (the sender)
	for _, id := range forwarded {
		if id == "peer-a" {
			t.Fatal("should not forward back to sender")
		}
	}
	if len(forwarded) == 0 {
		t.Fatal("expected at least one forward")
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG 'message received'")
	}
	if !capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("expected DEBUG 'message forwarded'")
	}
}

func TestEngineForwardDecrementHopCount(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var receivedData []byte
	var mu sync.Mutex

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-b", Send: func(data []byte) bool {
				mu.Lock()
				receivedData = data
				mu.Unlock()
				return true
			}},
		}
	})
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	engine.RegisterHandler(3, func(senderID string, payload []byte) {})

	env := makeSignedEnvelope(t, sender, 3, []byte("hello"), 5)
	engine.HandleIncoming("peer-a", env)

	mu.Lock()
	defer mu.Unlock()

	if receivedData == nil {
		t.Fatal("expected forwarded data")
	}

	var forwarded pb.Envelope
	if err := proto.Unmarshal(receivedData, &forwarded); err != nil {
		t.Fatalf("unmarshal forwarded: %v", err)
	}
	if forwarded.HopCount != 4 {
		t.Fatalf("expected hop_count=4, got %d", forwarded.HopCount)
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("expected DEBUG 'message forwarded'")
	}
}

func TestEngineBroadcast(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	var sentTo []string
	var mu sync.Mutex

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool {
				mu.Lock()
				sentTo = append(sentTo, "peer-a")
				mu.Unlock()
				return true
			}},
			{NodeID: "peer-b", Send: func(data []byte) bool {
				mu.Lock()
				sentTo = append(sentTo, "peer-b")
				mu.Unlock()
				return true
			}},
		}
	})

	env := makeSignedEnvelope(t, local, 3, []byte("hello"), 10)
	engine.Broadcast(env)

	mu.Lock()
	defer mu.Unlock()

	// Broadcast sends to ALL peers
	if len(sentTo) != 2 {
		t.Fatalf("expected 2 sends, got %d", len(sentTo))
	}

	// Log assertions: broadcast should not produce errors
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs during broadcast: %d", capture.Count(slog.LevelError))
	}
}

func TestEngineBroadcastMarksSeen(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })

	env := makeSignedEnvelope(t, local, 3, []byte("hello"), 10)
	engine.Broadcast(env)

	// The message should now be seen
	if !engine.Seen.Check(env.MessageId) {
		t.Fatal("expected broadcast message to be marked as seen")
	}

	// Log assertions
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", capture.Count(slog.LevelError))
	}
}

func TestEngineUnknownMsgType(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var forwarded int
	var mu sync.Mutex

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-b", Send: func(data []byte) bool {
				mu.Lock()
				forwarded++
				mu.Unlock()
				return true
			}},
		}
	})
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	// No handler registered for msg_type=999

	env := makeSignedEnvelope(t, sender, 999, []byte("plugin-data"), 5)
	engine.HandleIncoming("peer-a", env)

	mu.Lock()
	defer mu.Unlock()

	// Unknown types should still be forwarded
	if forwarded != 1 {
		t.Fatalf("expected 1 forward for unknown msg_type, got %d", forwarded)
	}

	// Log assertions: should forward but NOT deliver locally
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG 'message received'")
	}
	if !capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("expected DEBUG 'message forwarded'")
	}
	if capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("unexpected 'message delivered locally' for unregistered msg_type")
	}
}

func TestEngineRateLimiting(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	// Set very low rate limit
	engine.Limiter = NewRateLimiter(1.0) // 1/s, burst=2

	var delivered int
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	// Send 5 messages — only first 2 (burst) should be delivered
	for i := 0; i < 5; i++ {
		env := makeSignedEnvelope(t, sender, 3, []byte("msg"), 5)
		engine.HandleIncoming("peer-a", env)
	}

	if delivered > 2 {
		t.Fatalf("expected at most 2 deliveries (burst), got %d", delivered)
	}
	if delivered == 0 {
		t.Fatal("expected at least 1 delivery")
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "rate limit exceeded") {
		t.Error("expected DEBUG 'rate limit exceeded'")
	}
	if !capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("expected at least one DEBUG 'message delivered locally'")
	}
}

func TestEngineFanoutLimit(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var forwarded int
	var mu sync.Mutex

	// Create 10 peers
	peers := make([]PeerHandle, 10)
	for i := range peers {
		peers[i] = PeerHandle{
			NodeID: "peer-" + string(rune('A'+i)),
			Send: func(data []byte) bool {
				mu.Lock()
				forwarded++
				mu.Unlock()
				return true
			},
		}
	}

	engine := NewEngine(local.nodeID, func() []PeerHandle { return peers })
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	engine.RegisterHandler(3, func(senderID string, payload []byte) {})

	env := makeSignedEnvelope(t, sender, 3, []byte("hello"), 5)
	engine.HandleIncoming("external-peer", env)

	mu.Lock()
	defer mu.Unlock()

	// Fanout is 3 — should forward to at most 3 peers
	if forwarded > DefaultFanout {
		t.Fatalf("expected at most %d forwards, got %d", DefaultFanout, forwarded)
	}
	if forwarded == 0 {
		t.Fatal("expected at least 1 forward")
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG 'message received'")
	}
	if !capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("expected DEBUG 'message forwarded'")
	}
}

func TestEngineStartStop(t *testing.T) {
	local := newTestIdentity(t)
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })

	engine.Start()
	time.Sleep(50 * time.Millisecond)
	engine.Stop()
	// Should not panic or deadlock
}

func TestEngineAutoLearnKeyBadSignature(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	impostor := newTestIdentity(t)

	// Create a real nodeID from a different key pair
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(pub)
	realNodeID := hex.EncodeToString(hash[:])

	var delivered int
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	// sender_pubkey hash matches sender_id, but envelope is signed by impostor
	env := &pb.Envelope{
		MessageId:    uuid.New().String(),
		SenderId:     realNodeID,
		MsgType:      3,
		HopCount:     5,
		Payload:      []byte("hello"),
		SenderPubkey: pub,
	}
	pb.SignEnvelope(env, impostor.priv) // wrong key!

	engine.HandleIncoming("peer-a", env)

	if delivered != 0 {
		t.Fatal("expected no delivery when signature doesn't match sender_pubkey")
	}

	// Log assertions: key auto-learned (hash matches) but signature fails
	if !capture.Has(slog.LevelDebug, "auto-learned key") {
		t.Error("expected DEBUG 'auto-learned key'")
	}
	if !capture.Has(slog.LevelWarn, "invalid signature") {
		t.Error("expected WARN 'invalid signature'")
	}
	if capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("unexpected 'message delivered locally' with bad signature")
	}
}

func TestEngineHopCountZeroNotForwarded(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var delivered int
	var forwarded int

	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{{
			NodeID: "peer-b",
			Send:   func(data []byte) bool { forwarded++; return true },
		}}
	})
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	// hop_count=0: should still deliver locally but not forward
	env := makeSignedEnvelope(t, sender, 3, []byte("hello"), 0)
	engine.HandleIncoming("peer-a", env)

	if delivered != 1 {
		t.Fatalf("expected 1 delivery with hop_count=0, got %d", delivered)
	}
	if forwarded != 0 {
		t.Fatalf("expected no forwarding with hop_count=0, got %d", forwarded)
	}

	// Log assertions
	if !capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("expected DEBUG 'message delivered locally'")
	}
	if capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("unexpected 'message forwarded' with hop_count=0")
	}
}

func TestEngineDroppedMessageNotMarkedSeen(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	// Create a sender with a real nodeID = sha256(pubkey)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(pub)
	realNodeID := hex.EncodeToString(hash[:])

	var delivered int
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	env := &pb.Envelope{
		MessageId:    uuid.New().String(),
		SenderId:     realNodeID,
		MsgType:      3,
		HopCount:     5,
		Payload:      []byte("hello"),
		SenderPubkey: pub,
	}
	pb.SignEnvelope(env, priv)

	// First attempt: sender unknown (no sender_pubkey in this copy)
	envNoKey := &pb.Envelope{
		MessageId: env.MessageId,
		SenderId:  realNodeID,
		MsgType:   3,
		HopCount:  5,
		Payload:   []byte("hello"),
		Signature: env.Signature,
		// SenderPubkey intentionally omitted
	}
	engine.HandleIncoming("peer-a", envNoKey)
	if delivered != 0 {
		t.Fatal("expected no delivery for unknown sender without sender_pubkey")
	}

	// The message ID should NOT be in the seen cache
	if engine.Seen.Has(env.MessageId) {
		t.Fatal("dropped message should not be marked as seen")
	}

	// Second attempt: same message with sender_pubkey → should auto-learn and deliver
	engine.HandleIncoming("peer-a", env)
	if delivered != 1 {
		t.Fatalf("expected 1 delivery after retry with sender_pubkey, got %d", delivered)
	}

	// Log assertions
	if !capture.Has(slog.LevelWarn, "dropping message from unknown sender") {
		t.Error("expected WARN 'dropping message from unknown sender' on first attempt")
	}
	if !capture.Has(slog.LevelDebug, "auto-learned key") {
		t.Error("expected DEBUG 'auto-learned key' on second attempt")
	}
	if !capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("expected DEBUG 'message delivered locally' after retry")
	}
}

func TestEngineMaxHops(t *testing.T) {
	local := newTestIdentity(t)
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })

	if got := engine.MaxHops(); got != DefaultMaxHops {
		t.Fatalf("MaxHops() = %d, want %d", got, DefaultMaxHops)
	}
}

func TestEngineBroadcastNoPeers(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })

	env := makeSignedEnvelope(t, local, 3, []byte("hello"), 10)
	// Should not panic with zero peers
	engine.Broadcast(env)

	// Message should still be marked as seen
	if !engine.Seen.Has(env.MessageId) {
		t.Fatal("broadcast with no peers should still mark message as seen")
	}

	// Log assertions
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", capture.Count(slog.LevelError))
	}
}

func TestEngineForwardNoCandidates(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	sender := newTestIdentity(t)

	var forwarded int
	// Only peer is the one who sent us the message — no forwarding candidates
	engine := NewEngine(local.nodeID, func() []PeerHandle {
		return []PeerHandle{
			{NodeID: "peer-a", Send: func(data []byte) bool { forwarded++; return true }},
		}
	})
	engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
	engine.RegisterHandler(3, func(senderID string, payload []byte) {})

	env := makeSignedEnvelope(t, sender, 3, []byte("hello"), 5)
	engine.HandleIncoming("peer-a", env) // from peer-a, the only peer

	if forwarded != 0 {
		t.Fatalf("expected 0 forwards when only peer is sender, got %d", forwarded)
	}

	// Log assertions: received and delivered, but NOT forwarded
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG 'message received'")
	}
	if !capture.Has(slog.LevelDebug, "message delivered locally") {
		t.Error("expected DEBUG 'message delivered locally'")
	}
	if capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("unexpected 'message forwarded' with no candidates")
	}
}

func TestEngineAutoLearnKeyBadPubkeySize(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)

	var delivered int
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
	engine.RegisterHandler(3, func(senderID string, payload []byte) {
		delivered++
	})

	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	// sender_pubkey with wrong length (not 32 bytes)
	env := &pb.Envelope{
		MessageId:    uuid.New().String(),
		SenderId:     "unknown-sender",
		MsgType:      3,
		HopCount:     5,
		Payload:      []byte("hello"),
		SenderPubkey: []byte("too-short"),
	}
	pb.SignEnvelope(env, priv)
	engine.HandleIncoming("peer-a", env)

	if delivered != 0 {
		t.Fatal("expected no delivery with bad sender_pubkey size")
	}

	// Log assertions
	if !capture.Has(slog.LevelWarn, "dropping message from unknown sender") {
		t.Error("expected WARN 'dropping message from unknown sender'")
	}
}

func TestSeenCacheHasReadOnly(t *testing.T) {
	cache := NewSeenCache(time.Minute)

	// Has on unknown ID returns false and does NOT add it
	if cache.Has("msg-1") {
		t.Fatal("expected Has to return false for unknown ID")
	}
	if cache.Len() != 0 {
		t.Fatal("Has should not add entries to the cache")
	}

	// Check adds it
	if cache.Check("msg-1") {
		t.Fatal("expected Check to return false on first call")
	}
	if !cache.Has("msg-1") {
		t.Fatal("expected Has to return true after Check")
	}
}

func TestEngineEmptyEnvelope(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	local := newTestIdentity(t)
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })

	// Empty message_id and sender_id
	env := &pb.Envelope{MsgType: 3, HopCount: 5}
	engine.HandleIncoming("peer-a", env)

	// Log assertions
	if !capture.Has(slog.LevelDebug, "envelope rejected: empty id") {
		t.Error("expected DEBUG 'envelope rejected: empty id'")
	}
}

func TestEngineSizeValidation(t *testing.T) {
	tests := []struct {
		name     string
		payload  []byte
		mutate   func(*pb.Envelope)                       // post-sign mutation (ok for rejection tests)
		buildEnv func(*testing.T, testIdentity) *pb.Envelope // custom pre-sign envelope
		extraKR  func(*Engine, testIdentity)               // additional keyring entries
		wantDlv  int
		wantWarn string
		notSeen  bool
	}{
		{
			name: "oversized payload", payload: make([]byte, MaxGossipPayload+1),
			wantWarn: "envelope rejected: oversized payload", notSeen: true,
		},
		{
			name: "oversized message_id", payload: []byte("hello"),
			mutate:  func(env *pb.Envelope) { env.MessageId = strings.Repeat("x", MaxFieldLen+1) },
			wantWarn: "envelope rejected: oversized field", notSeen: true,
		},
		{
			name: "oversized sender_id", payload: []byte("hello"),
			mutate:  func(env *pb.Envelope) { env.SenderId = strings.Repeat("y", MaxFieldLen+1) },
			wantWarn: "envelope rejected: oversized field", notSeen: true,
		},
		{
			name: "payload at limit accepted", payload: make([]byte, MaxGossipPayload),
			wantDlv: 1,
		},
		{
			name: "message_id at limit accepted",
			buildEnv: func(t *testing.T, sender testIdentity) *pb.Envelope {
				t.Helper()
				env := &pb.Envelope{
					MessageId: strings.Repeat("m", MaxFieldLen),
					SenderId:  sender.nodeID,
					MsgType:   3, HopCount: 5,
					Payload: []byte("hello"),
				}
				pb.SignEnvelope(env, sender.priv)
				return env
			},
			wantDlv: 1,
		},
		{
			name: "sender_id at limit accepted",
			buildEnv: func(t *testing.T, sender testIdentity) *pb.Envelope {
				t.Helper()
				env := &pb.Envelope{
					MessageId: uuid.New().String(),
					SenderId:  strings.Repeat("s", MaxFieldLen),
					MsgType:   3, HopCount: 5,
					Payload: []byte("hello"),
				}
				pb.SignEnvelope(env, sender.priv)
				return env
			},
			extraKR: func(engine *Engine, sender testIdentity) {
				engine.KeyRing.Add(strings.Repeat("s", MaxFieldLen), sender.pub, TrustDirectlyVerified)
			},
			wantDlv: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := logging.CaptureForTest()
			defer capture.Restore()

			local := newTestIdentity(t)
			sender := newTestIdentity(t)

			var delivered int
			engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })
			engine.KeyRing.Add(sender.nodeID, sender.pub, TrustDirectlyVerified)
			if tt.extraKR != nil {
				tt.extraKR(engine, sender)
			}
			engine.RegisterHandler(3, func(_ string, _ []byte) { delivered++ })

			var env *pb.Envelope
			if tt.buildEnv != nil {
				env = tt.buildEnv(t, sender)
			} else {
				env = makeSignedEnvelope(t, sender, 3, tt.payload, 5)
				if tt.mutate != nil {
					tt.mutate(env)
				}
			}
			engine.HandleIncoming("peer-a", env)

			if delivered != tt.wantDlv {
				t.Fatalf("deliveries: got %d, want %d", delivered, tt.wantDlv)
			}
			if tt.wantWarn != "" && !capture.Has(slog.LevelWarn, tt.wantWarn) {
				t.Errorf("expected WARN %q", tt.wantWarn)
			}
			if tt.wantWarn == "" && capture.Count(slog.LevelWarn) != 0 {
				t.Error("unexpected WARN log")
			}
			if tt.notSeen && engine.Seen.Has(env.MessageId) {
				t.Fatal("rejected message should not be marked as seen")
			}
		})
	}
}

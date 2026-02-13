package gossip

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

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
}

func TestEngineUnknownSender(t *testing.T) {
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
}

func TestEngineAutoLearnKey(t *testing.T) {
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
}

func TestEngineAutoLearnKeyBadHash(t *testing.T) {
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
}

func TestEngineInvalidSignature(t *testing.T) {
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
}

func TestEngineHopCountZero(t *testing.T) {
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
}

func TestEngineForwarding(t *testing.T) {
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
}

func TestEngineForwardDecrementHopCount(t *testing.T) {
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
}

func TestEngineBroadcast(t *testing.T) {
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
}

func TestEngineBroadcastMarksSeen(t *testing.T) {
	local := newTestIdentity(t)

	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })

	env := makeSignedEnvelope(t, local, 3, []byte("hello"), 10)
	engine.Broadcast(env)

	// The message should now be seen
	if !engine.Seen.Check(env.MessageId) {
		t.Fatal("expected broadcast message to be marked as seen")
	}
}

func TestEngineUnknownMsgType(t *testing.T) {
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
}

func TestEngineRateLimiting(t *testing.T) {
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
}

func TestEngineFanoutLimit(t *testing.T) {
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
}

func TestEngineHopCountZeroNotForwarded(t *testing.T) {
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
}

func TestEngineDroppedMessageNotMarkedSeen(t *testing.T) {
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
}

func TestEngineMaxHops(t *testing.T) {
	local := newTestIdentity(t)
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })

	if got := engine.MaxHops(); got != DefaultMaxHops {
		t.Fatalf("MaxHops() = %d, want %d", got, DefaultMaxHops)
	}
}

func TestEngineBroadcastNoPeers(t *testing.T) {
	local := newTestIdentity(t)
	engine := NewEngine(local.nodeID, func() []PeerHandle { return nil })

	env := makeSignedEnvelope(t, local, 3, []byte("hello"), 10)
	// Should not panic with zero peers
	engine.Broadcast(env)

	// Message should still be marked as seen
	if !engine.Seen.Has(env.MessageId) {
		t.Fatal("broadcast with no peers should still mark message as seen")
	}
}

func TestEngineForwardNoCandidates(t *testing.T) {
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
}

func TestEngineAutoLearnKeyBadPubkeySize(t *testing.T) {
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

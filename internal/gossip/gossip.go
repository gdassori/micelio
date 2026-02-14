package gossip

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"micelio/internal/logging"
	pb "micelio/pkg/proto"
)

var logger = logging.For("gossip")

const (
	DefaultFanout    = 3
	DefaultMaxHops   = 10
	DefaultSeenTTL   = 5 * time.Minute
	DefaultMaxPerSec = 10.0
	seenCleanup      = 1 * time.Minute
	limiterCleanup   = 1 * time.Minute

	// MaxGossipPayload is the largest env.Payload the engine will accept.
	// Prevents amplification of near-frame-limit messages across the mesh.
	MaxGossipPayload = 64 << 10 // 64 KB

	// MaxFieldLen caps string identifier fields (MessageId, SenderId).
	MaxFieldLen = 128
)

// PeerHandle is a lightweight reference to a connected peer that the engine
// can send serialized envelopes to.
type PeerHandle struct {
	NodeID string
	Send   func([]byte) bool // returns false if buffer full
}

// MessageHandler processes a validated gossip message locally.
type MessageHandler func(senderID string, payload []byte)

// Engine is the gossip protocol engine. It handles deduplication, signature
// verification via keyring, rate limiting, hop count enforcement, local
// delivery to registered handlers, and random-subset forwarding.
type Engine struct {
	localID string

	Seen    *SeenCache
	KeyRing *KeyRing
	Limiter *RateLimiter

	fanout  int
	maxHops uint32

	// getPeers returns handles to all currently connected peers.
	getPeers func() []PeerHandle

	handlers map[uint32]MessageHandler

	done     chan struct{}
	stopOnce sync.Once

	// Statistics (atomic counters)
	statsReceived  atomic.Uint64 // messages received and validated
	statsDelivered atomic.Uint64 // messages delivered to local handlers
	statsBroadcast atomic.Uint64 // messages broadcast to peers
	statsForwarded atomic.Uint64 // messages forwarded to peers
	statsDropped   atomic.Uint64 // messages dropped (buffer full)
}

// NewEngine creates a gossip engine.
// localID is the local node's ID (excluded from forwarding targets).
// getPeers returns the current set of connected peers with send capabilities.
func NewEngine(localID string, getPeers func() []PeerHandle) *Engine {
	return &Engine{
		localID:  localID,
		Seen:     NewSeenCache(DefaultSeenTTL),
		KeyRing:  NewKeyRing(),
		Limiter:  NewRateLimiter(DefaultMaxPerSec),
		fanout:   DefaultFanout,
		maxHops:  DefaultMaxHops,
		getPeers: getPeers,
		handlers: make(map[uint32]MessageHandler),
		done:     make(chan struct{}),
	}
}

// Start launches background cleanup goroutines.
func (e *Engine) Start() {
	go e.Seen.CleanupLoop(e.done, seenCleanup)
	go e.Limiter.CleanupLoop(e.done, limiterCleanup)
}

// Stop shuts down the engine's background goroutines. Safe to call multiple times.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() { close(e.done) })
}

// MaxHops returns the default hop count for locally originated messages.
func (e *Engine) MaxHops() uint32 {
	return e.maxHops
}

// RegisterHandler registers a local handler for a message type.
// Messages with this type will be delivered to the handler after validation.
// Unknown message types are still forwarded via gossip but not processed locally.
// Must be called before Start() — not safe for concurrent use with HandleIncoming.
func (e *Engine) RegisterHandler(msgType uint32, handler MessageHandler) {
	e.handlers[msgType] = handler
}

// Stats represents aggregate gossip engine statistics.
// All counters track peer-level operations, not message-level.
// For example, broadcasting one message to 10 peers increments Broadcast by 10.
type Stats struct {
	Received  uint64 // unique messages received and validated (after dedup)
	Delivered uint64 // messages delivered to local handlers
	Broadcast uint64 // successful peer sends during broadcast operations
	Forwarded uint64 // successful peer sends during forward operations
	Dropped   uint64 // peer sends dropped due to full buffers
}

// Stats returns a snapshot of aggregate engine statistics.
func (e *Engine) Stats() Stats {
	return Stats{
		Received:  e.statsReceived.Load(),
		Delivered: e.statsDelivered.Load(),
		Broadcast: e.statsBroadcast.Load(),
		Forwarded: e.statsForwarded.Load(),
		Dropped:   e.statsDropped.Load(),
	}
}

// HandleIncoming processes a gossip message received from a connected peer.
// It performs deduplication, signature verification, rate limiting, hop count
// enforcement, local delivery, and forwarding.
func (e *Engine) HandleIncoming(fromPeerID string, env *pb.Envelope) {
	// 0a. Reject malformed envelopes missing required identifiers.
	if env.MessageId == "" || env.SenderId == "" {
		logger.Debug("envelope rejected: empty id")
		return
	}

	// 0b. Reject oversized identifier fields.
	if len(env.MessageId) > MaxFieldLen || len(env.SenderId) > MaxFieldLen {
		logger.Warn("envelope rejected: oversized field",
			"sender", formatShort(env.SenderId),
			"msg_type", env.MsgType,
			"msg_id_len", len(env.MessageId),
			"sender_id_len", len(env.SenderId))
		return
	}

	// 0c. Reject oversized payloads to prevent amplification attacks.
	if len(env.Payload) > MaxGossipPayload {
		logger.Warn("envelope rejected: oversized payload",
			"sender", formatShort(env.SenderId),
			"msg_type", env.MsgType,
			"size", len(env.Payload))
		return
	}

	// 1. Quick dedup (read-only; does NOT mark the ID as seen)
	if e.Seen.Has(env.MessageId) {
		return
	}

	logger.Debug("message received",
		"msg_id", env.MessageId,
		"sender", formatShort(env.SenderId),
		"hop_count", env.HopCount,
		"msg_type", env.MsgType)

	// 2. Lookup sender in keyring; auto-learn from sender_pubkey if unknown
	entry, known := e.KeyRing.Lookup(env.SenderId)
	if !known {
		// Try to learn the key from the envelope's sender_pubkey field.
		// Verify: len must be 32, and sha256(pubkey) must match sender_id.
		if len(env.SenderPubkey) == ed25519.PublicKeySize {
			hash := sha256.Sum256(env.SenderPubkey)
			if hex.EncodeToString(hash[:]) == env.SenderId {
				e.KeyRing.Add(env.SenderId, env.SenderPubkey, TrustGossipLearned)
				entry, known = e.KeyRing.Lookup(env.SenderId)
				logger.Debug("auto-learned key", "sender", formatShort(env.SenderId))
			}
		}
		if !known {
			logger.Warn("dropping message from unknown sender",
				"msg_id", env.MessageId,
				"sender", formatShort(env.SenderId))
			return
		}
	}

	// 3. Verify signature
	if !pb.VerifyEnvelope(env, entry.PubKey) {
		logger.Warn("invalid signature",
			"msg_id", env.MessageId,
			"sender", formatShort(env.SenderId))
		return
	}

	// 4. Rate limit (before marking seen, so rate-limited messages can be
	// retried when the sender's budget replenishes).
	if !e.Limiter.Allow(env.SenderId) {
		logger.Debug("rate limit exceeded", "sender", formatShort(env.SenderId))
		return
	}

	// 5. Atomic test-and-set AFTER verification and rate limiting succeed.
	// This ensures dropped messages (unknown sender, bad signature, rate-limited)
	// don't pollute the seen cache — they can be retried when conditions change.
	if e.Seen.Check(env.MessageId) {
		return
	}

	// Message successfully received and validated
	e.statsReceived.Add(1)

	// 6. Deliver to local handler (if registered for this msg_type)
	if handler, ok := e.handlers[env.MsgType]; ok {
		handler(env.SenderId, env.Payload)
		e.statsDelivered.Add(1)
		logger.Debug("message delivered locally",
			"msg_id", env.MessageId,
			"msg_type", env.MsgType)
	}

	// 7. Forward if hop count allows
	if env.HopCount <= 1 {
		return
	}

	// Decrement hop count and re-serialize
	env.HopCount--
	raw, err := proto.Marshal(env)
	if err != nil {
		logger.Error("marshal for forward", "err", err)
		return
	}

	e.forwardTo(raw, fromPeerID)
}

// Broadcast sends a locally originated, already-signed envelope to all
// connected peers. The caller is responsible for setting HopCount, signing
// the envelope, etc.
// Returns (sent, total) where sent is the number of peers that accepted the
// message and total is the number of peers attempted.
func (e *Engine) Broadcast(env *pb.Envelope) (sent, total int) {
	// Mark as seen so we don't re-process if it comes back
	e.Seen.Check(env.MessageId)

	raw, err := proto.Marshal(env)
	if err != nil {
		logger.Error("marshal broadcast", "err", err)
		return 0, 0
	}

	// Originator sends to ALL connected peers
	peers := e.getPeers()
	total = len(peers)

	for _, p := range peers {
		if p.Send(raw) {
			sent++
		}
	}

	e.statsBroadcast.Add(uint64(sent))

	dropped := total - sent
	if dropped > 0 {
		e.statsDropped.Add(uint64(dropped))
		logger.Warn("partial broadcast delivery",
			"sent", sent,
			"total", total,
			"dropped", dropped,
			"msg_id", env.MessageId)
	}

	return sent, total
}

// forwardTo sends serialized envelope bytes to a random subset of connected
// peers, excluding the peer that forwarded the message to us.
func (e *Engine) forwardTo(raw []byte, excludePeerID string) {
	peers := e.getPeers()

	var candidates []PeerHandle
	for _, p := range peers {
		if p.NodeID != excludePeerID {
			candidates = append(candidates, p)
		}
	}

	if len(candidates) == 0 {
		return
	}

	// Random subset selection (fanout)
	if len(candidates) > e.fanout {
		rand.Shuffle(len(candidates), func(i, j int) {
			candidates[i], candidates[j] = candidates[j], candidates[i]
		})
		candidates = candidates[:e.fanout]
	}

	total := len(candidates)
	var sent int

	for _, p := range candidates {
		if p.Send(raw) {
			sent++
		}
	}

	e.statsForwarded.Add(uint64(sent))
	dropped := total - sent
	if dropped > 0 {
		e.statsDropped.Add(uint64(dropped))
	}

	logger.Debug("message forwarded",
		"sent", sent,
		"total", total)
}

func formatShort(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

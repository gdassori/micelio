package transport

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	mcrypto "micelio/internal/crypto"
	"micelio/internal/identity"
	"micelio/internal/partyline"
	pb "micelio/pkg/proto"
)

// Message type constants.
const (
	MsgTypePeerHello         uint32 = 1
	MsgTypePeerHelloAck      uint32 = 2
	MsgTypeChat              uint32 = 3
	MsgTypePeerExchange      uint32 = 4
	MsgTypeKeepalive         uint32 = 5
	MsgTypeStateUpdate       uint32 = 6 // gossip-relayed state change
	MsgTypeStateSyncRequest  uint32 = 7 // direct peer-to-peer
	MsgTypeStateSyncResponse uint32 = 8 // direct peer-to-peer

	seenTTL     = 5 * time.Minute
	seenCleanup = 1 * time.Minute

	defaultIdleTimeout       = 60 * time.Second
	defaultKeepaliveInterval = 20 * time.Second
	defaultWriteTimeout      = 10 * time.Second
	defaultHandshakeTimeout  = 10 * time.Second
)

// Peer represents a connection to a single remote node.
type Peer struct {
	NodeID   string
	edPubKey ed25519.PublicKey

	conn       *noiseConn
	peerX25519 []byte // from Noise handshake
	localID    *identity.Identity
	hub        *partyline.Hub
	initiator  bool

	// Metadata from PeerHello exchange
	remoteReachable  bool
	remoteListenAddr string

	// Callback for PeerExchange messages (set by Manager)
	onPeerExchange func(nodeID string, infos []*pb.PeerInfo)

	// Callback for gossip-eligible messages (set by Manager).
	// All msg_types except PeerHello/HelloAck/PeerExchange/StateSync are routed here.
	onGossipMessage func(fromPeerID string, env *pb.Envelope)

	// Callbacks for state sync messages (set by Manager).
	onStateSyncRequest  func(fromPeerID string)
	onStateSyncResponse func(fromPeerID string, entries []*pb.StateEntry)

	sendCh chan []byte // framed envelope bytes ready to write
	done   chan struct{}

	idleTimeout       time.Duration
	keepaliveInterval time.Duration
	writeTimeout      atomic.Int64 // nanoseconds; use atomics for concurrent access
	handshakeTimeout  time.Duration

	seen   map[string]time.Time
	seenMu sync.Mutex

	closeOnce sync.Once
}

func newPeer(conn *noiseConn, peerX25519 []byte, localID *identity.Identity, hub *partyline.Hub, initiator bool) *Peer {
	p := &Peer{
		conn:              conn,
		peerX25519:        peerX25519,
		localID:           localID,
		hub:               hub,
		initiator:         initiator,
		sendCh:            make(chan []byte, 64),
		done:              make(chan struct{}),
		idleTimeout:       defaultIdleTimeout,
		keepaliveInterval: defaultKeepaliveInterval,
		handshakeTimeout:  defaultHandshakeTimeout,
		seen:              make(map[string]time.Time),
	}
	p.writeTimeout.Store(int64(defaultWriteTimeout))
	return p
}

// ExchangeHello performs the PeerHello / PeerHelloAck exchange and verifies
// the peer's identity against the Noise-authenticated X25519 key.
// A handshake timeout prevents a slow or malicious peer from blocking the
// goroutine indefinitely.
func (p *Peer) ExchangeHello(localCfg HelloConfig) error {
	// Set a deadline for the entire hello exchange. Cleared on success so
	// it doesn't interfere with the subsequent send/recv loops.
	deadline := time.Now().Add(p.handshakeTimeout)
	if err := p.conn.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("set handshake read deadline: %w", err)
	}
	if err := p.conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set handshake write deadline: %w", err)
	}
	defer func() {
		_ = p.conn.SetReadDeadline(time.Time{})
		_ = p.conn.SetWriteDeadline(time.Time{})
	}()

	hello := &pb.PeerHello{
		NodeId:        p.localID.NodeID,
		Version:       "0.1.0",
		Tags:          localCfg.Tags,
		Reachable:     localCfg.Reachable,
		ListenAddr:    localCfg.ListenAddr,
		Ed25519Pubkey: p.localID.PublicKey,
	}

	if p.initiator {
		// Initiator sends PeerHello, waits for PeerHelloAck
		if err := p.sendHello(hello, MsgTypePeerHello); err != nil {
			return err
		}
		ack, err := p.recvHello(MsgTypePeerHelloAck)
		if err != nil {
			return err
		}
		return p.verifyPeerIdentity(ack)
	}

	// Responder waits for PeerHello, sends PeerHelloAck
	peerHello, err := p.recvHello(MsgTypePeerHello)
	if err != nil {
		return err
	}
	if err := p.verifyPeerIdentity(peerHello); err != nil {
		return err
	}
	return p.sendHello(hello, MsgTypePeerHelloAck)
}

// Run starts the send and receive loops. Blocks until the connection is closed.
func (p *Peer) Run() {
	go p.cleanupSeenLoop()

	done := make(chan error, 2)
	go func() { done <- p.sendLoop() }()
	go func() { done <- p.recvLoop() }()

	// Wait for either loop to finish, then shut down
	<-done
	p.Close()
	<-done
}

// SendRaw queues pre-serialized envelope bytes for sending to this peer.
// Returns false if the send buffer is full (message dropped).
func (p *Peer) SendRaw(data []byte) bool {
	select {
	case p.sendCh <- data:
		return true
	default:
		tlog.Warn("send buffer full", "peer", formatNodeIDShort(p.NodeID))
		return false
	}
}

// Close shuts down the peer connection.
func (p *Peer) Close() {
	p.closeOnce.Do(func() {
		close(p.done)
		_ = p.conn.Close()
	})
}

func (p *Peer) sendWithDeadline(data []byte) error {
	if err := p.conn.SetWriteDeadline(time.Now().Add(time.Duration(p.writeTimeout.Load()))); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	if err := WriteFrame(p.conn, data); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			tlog.Warn("peer write timeout", "peer", formatNodeIDShort(p.NodeID))
		}
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

func (p *Peer) sendLoop() error {
	timer := time.NewTimer(p.keepaliveInterval)
	defer timer.Stop()
	for {
		select {
		case data := <-p.sendCh:
			if err := p.sendWithDeadline(data); err != nil {
				return err
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(p.keepaliveInterval)
		case <-timer.C:
			if err := p.writeKeepalive(); err != nil {
				return err
			}
			timer.Reset(p.keepaliveInterval)
		case <-p.done:
			return nil
		}
	}
}

func (p *Peer) writeKeepalive() error {
	env := &pb.Envelope{MsgType: MsgTypeKeepalive}
	data, err := proto.Marshal(env)
	if err != nil {
		return err
	}
	return p.sendWithDeadline(data)
}

func (p *Peer) recvLoop() error {
	for {
		if err := p.conn.SetReadDeadline(time.Now().Add(p.idleTimeout)); err != nil {
			select {
			case <-p.done:
				return nil
			default:
				return fmt.Errorf("set read deadline: %w", err)
			}
		}
		data, err := ReadFrame(p.conn)
		if err != nil {
			select {
			case <-p.done:
				return nil
			default:
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					tlog.Warn("peer idle timeout", "peer", formatNodeIDShort(p.NodeID))
					return err
				}
				return fmt.Errorf("read frame: %w", err)
			}
		}

		var env pb.Envelope
		if err := proto.Unmarshal(data, &env); err != nil {
			tlog.Error("unmarshal envelope", "peer", formatNodeIDShort(p.NodeID), "err", err)
			continue
		}

		switch env.MsgType {
		case MsgTypeKeepalive:
			// Keepalive resets the read deadline (already done above); no further processing.
			continue

		case MsgTypePeerExchange:
			// Direct peer-to-peer message: per-peer dedup + signature check.
			if p.isDuplicate(env.MessageId) {
				continue
			}
			if !verifySignature(&env, p.edPubKey) {
				tlog.Warn("invalid signature on peer_exchange", "peer", formatNodeIDShort(p.NodeID), "msg_id", env.MessageId)
				continue
			}
			if env.SenderId != p.NodeID {
				tlog.Warn("sender_id mismatch on peer_exchange", "peer", formatNodeIDShort(p.NodeID), "claimed", env.SenderId)
				continue
			}
			p.handlePeerExchange(&env)

		case MsgTypeStateSyncRequest:
			// Direct peer-to-peer: per-peer dedup + signature check.
			if p.isDuplicate(env.MessageId) {
				continue
			}
			if !verifySignature(&env, p.edPubKey) {
				tlog.Warn("invalid signature on state_sync_request", "peer", formatNodeIDShort(p.NodeID), "msg_id", env.MessageId)
				continue
			}
			if env.SenderId != p.NodeID {
				tlog.Warn("sender_id mismatch on state_sync_request", "peer", formatNodeIDShort(p.NodeID), "claimed", env.SenderId)
				continue
			}
			if p.onStateSyncRequest != nil {
				p.onStateSyncRequest(p.NodeID)
			}

		case MsgTypeStateSyncResponse:
			// Direct peer-to-peer: per-peer dedup + signature check.
			if p.isDuplicate(env.MessageId) {
				continue
			}
			if !verifySignature(&env, p.edPubKey) {
				tlog.Warn("invalid signature on state_sync_response", "peer", formatNodeIDShort(p.NodeID), "msg_id", env.MessageId)
				continue
			}
			if env.SenderId != p.NodeID {
				tlog.Warn("sender_id mismatch on state_sync_response", "peer", formatNodeIDShort(p.NodeID), "claimed", env.SenderId)
				continue
			}
			var resp pb.StateSyncResponse
			if err := proto.Unmarshal(env.Payload, &resp); err != nil {
				tlog.Error("unmarshal state_sync_response", "peer", formatNodeIDShort(p.NodeID), "err", err)
				continue
			}
			if p.onStateSyncResponse != nil {
				p.onStateSyncResponse(p.NodeID, resp.Entries)
			}

		case MsgTypePeerHello, MsgTypePeerHelloAck:
			// Handshake messages are point-to-point only; never gossip-relay them.
			continue

		default:
			// All other message types are gossip-eligible:
			// route to the gossip engine for dedup, signature verification
			// via keyring, rate limiting, local delivery, and forwarding.
			if p.onGossipMessage != nil {
				p.onGossipMessage(p.NodeID, &env)
			}
		}
	}
}

func (p *Peer) handlePeerExchange(env *pb.Envelope) {
	var px pb.PeerExchange
	if err := proto.Unmarshal(env.Payload, &px); err != nil {
		tlog.Error("unmarshal peer_exchange", "peer", formatNodeIDShort(p.NodeID), "err", err)
		return
	}
	if p.onPeerExchange != nil {
		p.onPeerExchange(p.NodeID, px.Peers)
	}
}

func (p *Peer) sendHello(hello *pb.PeerHello, msgType uint32) error {
	payload, err := proto.Marshal(hello)
	if err != nil {
		return fmt.Errorf("marshal hello: %w", err)
	}

	env := &pb.Envelope{
		MessageId: uuid.New().String(),
		SenderId:  p.localID.NodeID,
		MsgType:   msgType,
		HopCount:  1,
		Payload:   payload,
	}
	signEnvelope(env, p.localID.PrivateKey)

	envBytes, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	return WriteFrame(p.conn, envBytes)
}

func (p *Peer) recvHello(expectedType uint32) (*pb.PeerHello, error) {
	data, err := ReadFrame(p.conn)
	if err != nil {
		return nil, fmt.Errorf("read hello frame: %w", err)
	}

	var env pb.Envelope
	if err := proto.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("unmarshal hello envelope: %w", err)
	}
	if env.MsgType != expectedType {
		return nil, fmt.Errorf("expected msg_type %d, got %d", expectedType, env.MsgType)
	}

	var hello pb.PeerHello
	if err := proto.Unmarshal(env.Payload, &hello); err != nil {
		return nil, fmt.Errorf("unmarshal hello: %w", err)
	}

	// Verify envelope signature using the public key from the hello message
	pubKey := ed25519.PublicKey(hello.Ed25519Pubkey)
	if !verifySignature(&env, pubKey) {
		return nil, fmt.Errorf("invalid signature on hello from %s", hello.NodeId)
	}

	// Ensure envelope sender_id is consistent with hello node_id
	if env.SenderId != hello.NodeId {
		return nil, fmt.Errorf("sender_id mismatch: envelope %q vs hello %q", env.SenderId, hello.NodeId)
	}

	return &hello, nil
}

func (p *Peer) verifyPeerIdentity(hello *pb.PeerHello) error {
	pubKey := ed25519.PublicKey(hello.Ed25519Pubkey)

	// 1. node_id must match hash of public key
	hash := sha256.Sum256(pubKey)
	expectedNodeID := hex.EncodeToString(hash[:])
	if hello.NodeId != expectedNodeID {
		return fmt.Errorf("node_id mismatch: claimed %s, derived %s", hello.NodeId, expectedNodeID)
	}

	// 2. ED25519 pubkey → X25519 must match Noise peer static
	x25519Pub, err := mcrypto.EdPublicToX25519(pubKey)
	if err != nil {
		return fmt.Errorf("converting peer ed25519 to x25519: %w", err)
	}
	if !bytes.Equal(x25519Pub, p.peerX25519) {
		return fmt.Errorf("x25519 key mismatch: hello key does not match noise handshake key")
	}

	p.NodeID = hello.NodeId
	p.edPubKey = pubKey
	p.remoteReachable = hello.Reachable
	p.remoteListenAddr = hello.ListenAddr
	return nil
}

func (p *Peer) isDuplicate(messageID string) bool {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	if _, ok := p.seen[messageID]; ok {
		return true
	}
	p.seen[messageID] = time.Now()
	return false
}

func (p *Peer) cleanupSeenLoop() {
	ticker := time.NewTicker(seenCleanup)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.seenMu.Lock()
			cutoff := time.Now().Add(-seenTTL)
			for id, t := range p.seen {
				if t.Before(cutoff) {
					delete(p.seen, id)
				}
			}
			p.seenMu.Unlock()
		case <-p.done:
			return
		}
	}
}

// HelloConfig contains local node info for the PeerHello exchange.
type HelloConfig struct {
	Tags       []string
	Reachable  bool
	ListenAddr string
}

// Wrappers around shared sign/verify helpers from pkg/proto.
func signEnvelope(env *pb.Envelope, privKey ed25519.PrivateKey) {
	pb.SignEnvelope(env, privKey)
}

func verifySignature(env *pb.Envelope, pubKey ed25519.PublicKey) bool {
	return pb.VerifyEnvelope(env, pubKey)
}


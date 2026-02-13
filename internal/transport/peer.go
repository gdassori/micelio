package transport

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
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
	MsgTypePeerHello    uint32 = 1
	MsgTypePeerHelloAck uint32 = 2
	MsgTypeChat         uint32 = 3
	MsgTypePeerExchange uint32 = 4

	seenTTL     = 5 * time.Minute
	seenCleanup = 1 * time.Minute
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
	// All msg_types except PeerHello/HelloAck/PeerExchange are routed here.
	onGossipMessage func(fromPeerID string, env *pb.Envelope)

	sendCh chan []byte // framed envelope bytes ready to write
	done   chan struct{}

	seen   map[string]time.Time
	seenMu sync.Mutex

	closeOnce sync.Once
}

func newPeer(conn *noiseConn, peerX25519 []byte, localID *identity.Identity, hub *partyline.Hub, initiator bool) *Peer {
	return &Peer{
		conn:       conn,
		peerX25519: peerX25519,
		localID:    localID,
		hub:        hub,
		initiator:  initiator,
		sendCh:     make(chan []byte, 64),
		done:       make(chan struct{}),
		seen:       make(map[string]time.Time),
	}
}

// ExchangeHello performs the PeerHello / PeerHelloAck exchange and verifies
// the peer's identity against the Noise-authenticated X25519 key.
func (p *Peer) ExchangeHello(localCfg HelloConfig) error {
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
		log.Printf("peer %s: send buffer full, dropping message", p.NodeID)
		return false
	}
}

// Close shuts down the peer connection.
func (p *Peer) Close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.conn.Close()
	})
}

func (p *Peer) sendLoop() error {
	for {
		select {
		case data := <-p.sendCh:
			if err := WriteFrame(p.conn, data); err != nil {
				return fmt.Errorf("write frame: %w", err)
			}
		case <-p.done:
			return nil
		}
	}
}

func (p *Peer) recvLoop() error {
	for {
		data, err := ReadFrame(p.conn)
		if err != nil {
			select {
			case <-p.done:
				return nil
			default:
				return fmt.Errorf("read frame: %w", err)
			}
		}

		var env pb.Envelope
		if err := proto.Unmarshal(data, &env); err != nil {
			log.Printf("peer %s: unmarshal envelope: %v", p.NodeID, err)
			continue
		}

		switch env.MsgType {
		case MsgTypePeerExchange:
			// Direct peer-to-peer message: per-peer dedup + signature check.
			if p.isDuplicate(env.MessageId) {
				continue
			}
			if !verifySignature(&env, p.edPubKey) {
				log.Printf("peer %s: invalid signature on peer_exchange %s", p.NodeID, env.MessageId)
				continue
			}
			if env.SenderId != p.NodeID {
				log.Printf("peer %s: sender_id mismatch on peer_exchange: claimed %s", p.NodeID, env.SenderId)
				continue
			}
			p.handlePeerExchange(&env)

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
		log.Printf("peer %s: unmarshal peer_exchange: %v", p.NodeID, err)
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


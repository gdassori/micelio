package transport

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/flynn/noise"

	mcrypto "micelio/internal/crypto"
	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/partyline"
)

// Manager manages all peer connections for a node.
type Manager struct {
	cfg      *config.Config
	id       *identity.Identity
	hub      *partyline.Hub
	noiseKey noise.DHKey

	mu         sync.Mutex
	peers      map[string]*Peer // nodeID → Peer
	listener   net.Listener
	listenAddr string // actual bound address (set after Listen)

	remoteSend chan partyline.RemoteMsg
	done       chan struct{}
	closeOnce  sync.Once
}

// NewManager creates a transport manager.
// It derives X25519 keys from the identity and registers with the hub for outgoing messages.
func NewManager(cfg *config.Config, id *identity.Identity, hub *partyline.Hub) (*Manager, error) {
	noiseKey, err := mcrypto.EdToNoiseKeypair(id)
	if err != nil {
		return nil, fmt.Errorf("deriving noise keypair: %w", err)
	}

	remoteSend := make(chan partyline.RemoteMsg, 64)
	hub.SetRemoteSend(remoteSend)

	return &Manager{
		cfg:        cfg,
		id:         id,
		hub:        hub,
		noiseKey:   noiseKey,
		peers:      make(map[string]*Peer),
		remoteSend: remoteSend,
		done:       make(chan struct{}),
	}, nil
}

// Start begins listening for inbound connections and dialing bootstrap peers.
func (m *Manager) Start(ctx context.Context) error {
	if m.cfg.Network.Listen != "" {
		ln, err := net.Listen("tcp", m.cfg.Network.Listen)
		if err != nil {
			return fmt.Errorf("transport listen: %w", err)
		}
		m.mu.Lock()
		m.listener = ln
		m.listenAddr = ln.Addr().String()
		m.mu.Unlock()
		go m.listenLoop(ctx)
	}

	for _, addr := range m.cfg.Network.Bootstrap {
		go m.dialWithBackoff(ctx, addr)
	}

	go m.fanoutLoop(ctx)

	<-ctx.Done()
	return nil
}

// Stop shuts down the manager, closing all connections.
func (m *Manager) Stop() {
	m.closeOnce.Do(func() {
		close(m.done)
		m.mu.Lock()
		if m.listener != nil {
			m.listener.Close()
		}
		for _, p := range m.peers {
			p.Close()
		}
		m.mu.Unlock()
	})
}

// Addr returns the listener's address. Empty if not listening.
func (m *Manager) Addr() string {
	m.mu.Lock()
	ln := m.listener
	m.mu.Unlock()
	if ln == nil {
		return ""
	}
	return ln.Addr().String()
}

// PeerCount returns the number of connected peers.
func (m *Manager) PeerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.peers)
}

func (m *Manager) listenLoop(ctx context.Context) {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-m.done:
				return
			default:
				log.Printf("transport accept: %v", err)
				continue
			}
		}
		go m.handleInbound(conn)
	}
}

func (m *Manager) handleInbound(conn net.Conn) {
	nc, peerX25519, err := Handshake(conn, false, m.noiseKey)
	if err != nil {
		log.Printf("inbound noise handshake from %s: %v", conn.RemoteAddr(), err)
		conn.Close()
		return
	}

	peer := newPeer(nc, peerX25519, m.id, m.hub, false)
	if err := peer.ExchangeHello(m.helloConfig()); err != nil {
		log.Printf("inbound hello from %s: %v", conn.RemoteAddr(), err)
		nc.Close()
		return
	}

	if !m.addPeer(peer) {
		log.Printf("duplicate peer %s, dropping inbound", peer.NodeID)
		nc.Close()
		return
	}

	log.Printf("peer connected (inbound): %s", peer.NodeID)
	peer.Run()
	m.removePeer(peer.NodeID)
	log.Printf("peer disconnected: %s", peer.NodeID)
}

func (m *Manager) dialWithBackoff(ctx context.Context, addr string) {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		default:
		}

		if err := m.dial(addr); err != nil {
			log.Printf("dial %s: %v", addr, err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			case <-m.done:
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Connected and ran until disconnect — reset backoff and reconnect
		backoff = time.Second
	}
}

func (m *Manager) dial(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}

	nc, peerX25519, err := Handshake(conn, true, m.noiseKey)
	if err != nil {
		conn.Close()
		return fmt.Errorf("noise handshake: %w", err)
	}

	peer := newPeer(nc, peerX25519, m.id, m.hub, true)
	if err := peer.ExchangeHello(m.helloConfig()); err != nil {
		nc.Close()
		return fmt.Errorf("hello exchange: %w", err)
	}

	if !m.addPeer(peer) {
		nc.Close()
		return fmt.Errorf("duplicate peer %s", peer.NodeID)
	}

	log.Printf("peer connected (outbound): %s", peer.NodeID)
	peer.Run()
	m.removePeer(peer.NodeID)
	log.Printf("peer disconnected: %s", peer.NodeID)
	return nil
}

func (m *Manager) fanoutLoop(ctx context.Context) {
	for {
		select {
		case msg := <-m.remoteSend:
			m.mu.Lock()
			for _, p := range m.peers {
				p.SendChat(msg.Nick, msg.Text)
			}
			m.mu.Unlock()
		case <-ctx.Done():
			return
		case <-m.done:
			return
		}
	}
}

func (m *Manager) addPeer(p *Peer) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.peers[p.NodeID]; exists {
		return false
	}
	m.peers[p.NodeID] = p
	return true
}

func (m *Manager) removePeer(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.peers, nodeID)
}

func (m *Manager) helloConfig() HelloConfig {
	m.mu.Lock()
	addr := m.listenAddr
	m.mu.Unlock()
	return HelloConfig{
		Tags:       m.cfg.Node.Tags,
		Reachable:  addr != "",
		ListenAddr: addr,
	}
}

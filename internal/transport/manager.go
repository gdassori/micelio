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
	"micelio/internal/store"
)

// Manager manages all peer connections for a node.
type Manager struct {
	cfg      *config.Config
	id       *identity.Identity
	hub      *partyline.Hub
	noiseKey noise.DHKey
	store    store.Store // nil in tests

	mu             sync.Mutex
	peers          map[string]*Peer       // nodeID → Peer
	knownPeers     map[string]*PeerRecord // discovered peers
	dialing        map[string]bool        // nodeIDs being dialed
	bootstrapAddrs map[string]bool        // addresses managed by dialWithBackoff
	listener       net.Listener
	listenAddr     string // actual bound address (set after Listen)

	remoteSend chan partyline.RemoteMsg
	done       chan struct{}
	closeOnce  sync.Once
}

// NewManager creates a transport manager.
// It derives X25519 keys from the identity and registers with the hub for outgoing messages.
// The store parameter may be nil (e.g. in tests); discovery works in-memory only.
func NewManager(cfg *config.Config, id *identity.Identity, hub *partyline.Hub, st store.Store) (*Manager, error) {
	noiseKey, err := mcrypto.EdToNoiseKeypair(id)
	if err != nil {
		return nil, fmt.Errorf("deriving noise keypair: %w", err)
	}

	remoteSend := make(chan partyline.RemoteMsg, 64)
	hub.SetRemoteSend(remoteSend)

	bootstrapSet := make(map[string]bool, len(cfg.Network.Bootstrap))
	for _, addr := range cfg.Network.Bootstrap {
		bootstrapSet[addr] = true
	}

	mgr := &Manager{
		cfg:            cfg,
		id:             id,
		hub:            hub,
		noiseKey:       noiseKey,
		store:          st,
		peers:          make(map[string]*Peer),
		knownPeers:     make(map[string]*PeerRecord),
		dialing:        make(map[string]bool),
		bootstrapAddrs: bootstrapSet,
		remoteSend:     remoteSend,
		done:           make(chan struct{}),
	}

	mgr.loadKnownPeers()

	return mgr, nil
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
	go m.exchangeLoop(ctx)
	go m.discoveryLoop(ctx)

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
	// Quick capacity check before expensive handshake.
	// The definitive check is in addPeer; this avoids wasted crypto work.
	if m.cfg.Network.MaxPeers > 0 {
		m.mu.Lock()
		atCapacity := len(m.peers) >= m.cfg.Network.MaxPeers
		m.mu.Unlock()
		if atCapacity {
			log.Printf("rejecting inbound connection from %s: at MaxPeers capacity", conn.RemoteAddr())
			conn.Close()
			return
		}
	}

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

	if reason := m.addPeer(peer); reason != "" {
		log.Printf("rejecting inbound peer %s: %s", peer.NodeID, reason)
		nc.Close()
		return
	}

	log.Printf("peer connected (inbound): %s", peer.NodeID)
	m.onPeerConnected(peer)
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

	if reason := m.addPeer(peer); reason != "" {
		nc.Close()
		return fmt.Errorf("peer %s rejected: %s", peer.NodeID, reason)
	}

	log.Printf("peer connected (outbound): %s", peer.NodeID)
	m.onPeerConnected(peer)
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

// addPeer adds a peer to the peers map. Returns "" on success, or a reason on failure.
func (m *Manager) addPeer(p *Peer) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.peers[p.NodeID]; exists {
		return "duplicate"
	}
	if m.cfg.Network.MaxPeers > 0 && len(m.peers) >= m.cfg.Network.MaxPeers {
		return "max peers reached"
	}
	p.onPeerExchange = m.peerExchangeHandler()
	m.peers[p.NodeID] = p
	return ""
}

func (m *Manager) removePeer(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.peers, nodeID)
}

func (m *Manager) helloConfig() HelloConfig {
	addr := m.advertiseAddr()
	return HelloConfig{
		Tags:       m.cfg.Node.Tags,
		Reachable:  addr != "",
		ListenAddr: addr,
	}
}

// advertiseAddr returns the address to advertise to peers.
// It prefers cfg.Network.AdvertiseAddr, then falls back to the bound
// listen address — but only if the listen host is not a wildcard.
func (m *Manager) advertiseAddr() string {
	if a := m.cfg.Network.AdvertiseAddr; a != "" {
		return a
	}
	m.mu.Lock()
	addr := m.listenAddr
	m.mu.Unlock()
	if addr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// If the listen address is not in host:port form, don't advertise it.
		return ""
	}
	// Don't advertise wildcard addresses — peers can't reach 0.0.0.0.
	if host == "0.0.0.0" || host == "::" || host == "" {
		return ""
	}
	return addr
}

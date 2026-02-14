package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/flynn/noise"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"micelio/internal/config"
	mcrypto "micelio/internal/crypto"
	"micelio/internal/gossip"
	"micelio/internal/identity"
	"micelio/internal/logging"
	"micelio/internal/partyline"
	"micelio/internal/state"
	"micelio/internal/store"
	pb "micelio/pkg/proto"
)

var tlog = logging.For("transport")

// Manager manages all peer connections for a node.
type Manager struct {
	cfg      *config.Config
	id       *identity.Identity
	hub      *partyline.Hub
	noiseKey noise.DHKey
	store    store.Store // nil in tests
	gossip   *gossip.Engine
	stateMap *state.Map // distributed state map

	mu             sync.Mutex
	peers          map[string]*Peer       // nodeID → Peer
	knownPeers     map[string]*PeerRecord // discovered peers
	dialing        map[string]bool        // nodeIDs being dialed
	bootstrapAddrs map[string]bool        // addresses managed by dialWithBackoff
	listener       net.Listener
	listenAddr     string // actual bound address (set after Listen)

	remoteSend chan partyline.RemoteMsg
	persistCh  chan state.Entry // buffered channel for write-behind persistence
	done       chan struct{}
	closeOnce  sync.Once

	// Peer timeout overrides (0 = use peer defaults). Set before Start().
	peerIdleTimeout       time.Duration
	peerKeepaliveInterval time.Duration
	peerWriteTimeout      time.Duration
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
		persistCh:      make(chan state.Entry, 256),
		done:           make(chan struct{}),
	}

	// Create gossip engine — the manager provides peer handles via callback.
	mgr.gossip = gossip.NewEngine(id.NodeID, mgr.peerHandles)

	// Register chat message handler: deliver to local partyline hub.
	mgr.gossip.RegisterHandler(MsgTypeChat, func(senderID string, payload []byte) {
		var chat pb.ChatMessage
		if err := proto.Unmarshal(payload, &chat); err != nil {
			tlog.Error("unmarshal chat", "err", err)
			return
		}
		hub.DeliverRemote(chat.Nick, chat.Text)
	})

	// Add self to keyring so our own messages (reflected via gossip) can be verified.
	mgr.gossip.KeyRing.Add(id.NodeID, id.PublicKey, gossip.TrustDirectlyVerified)

	// Create the distributed state map and load persisted state.
	stateMap := state.NewMap(id.NodeID)
	if err := stateMap.LoadFromStore(st); err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}
	mgr.stateMap = stateMap

	// Broadcast state changes via gossip when local writes win LWW.
	stateMap.SetChangeHandler(func(entry state.Entry) {
		update := &pb.StateUpdate{
			Entry: &pb.StateEntry{
				Key:       entry.Key,
				Value:     entry.Value,
				LamportTs: entry.LamportTs,
				NodeId:    entry.NodeID,
			},
		}
		payload, err := proto.Marshal(update)
		if err != nil {
			tlog.Error("marshal state_update", "err", err)
			return
		}
		env := &pb.Envelope{
			MessageId:    uuid.New().String(),
			SenderId:     id.NodeID,
			MsgType:      MsgTypeStateUpdate,
			HopCount:     mgr.gossip.MaxHops(),
			Payload:      payload,
			SenderPubkey: id.PublicKey,
		}
		pb.SignEnvelope(env, id.PrivateKey)
		mgr.gossip.Broadcast(env)
		mgr.enqueuePersist(entry)
	})

	// Register state update handler: merge remote updates into local map.
	mgr.gossip.RegisterHandler(MsgTypeStateUpdate, func(senderID string, payload []byte) {
		var update pb.StateUpdate
		if err := proto.Unmarshal(payload, &update); err != nil {
			tlog.Error("unmarshal state_update", "err", err)
			return
		}
		if update.Entry == nil {
			tlog.Warn("state_update with nil entry", "sender", formatNodeIDShort(senderID))
			return
		}
		entry := state.Entry{
			Key:       update.Entry.Key,
			Value:     update.Entry.Value,
			LamportTs: update.Entry.LamportTs,
			NodeID:    update.Entry.NodeId,
		}
		if mgr.stateMap.Merge(entry) {
			mgr.enqueuePersist(entry)
		}
	})

	mgr.loadKnownPeers()

	return mgr, nil
}

// Start begins listening for inbound connections and dialing bootstrap peers.
func (m *Manager) Start(ctx context.Context) error {
	m.gossip.Start()

	if m.cfg.Network.Listen != "" {
		ln, err := net.Listen("tcp", m.cfg.Network.Listen)
		if err != nil {
			m.gossip.Stop()
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

	if m.store != nil {
		go m.persistLoop()
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
		m.gossip.Stop()
		m.mu.Lock()
		if m.listener != nil {
			_ = m.listener.Close()
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

// StateMap returns the distributed state map. May be nil if not initialized.
func (m *Manager) StateMap() *state.Map {
	return m.stateMap
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
				tlog.Warn("accept error", "err", err)
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
			tlog.Info("rejecting inbound: at capacity", "remote", conn.RemoteAddr())
			_ = conn.Close()
			return
		}
	}

	nc, peerX25519, err := Handshake(conn, false, m.noiseKey)
	if err != nil {
		tlog.Warn("inbound handshake failed", "remote", conn.RemoteAddr(), "err", err)
		_ = conn.Close()
		return
	}

	peer := newPeer(nc, peerX25519, m.id, m.hub, false)
	m.applyPeerTimeouts(peer)
	if err := peer.ExchangeHello(m.helloConfig()); err != nil {
		tlog.Warn("inbound hello failed", "remote", conn.RemoteAddr(), "err", err)
		_ = nc.Close()
		return
	}

	if reason := m.addPeer(peer); reason != "" {
		tlog.Info("rejecting inbound peer", "peer", peer.NodeID, "reason", reason)
		_ = nc.Close()
		return
	}

	tlog.Info("peer connected", "peer", peer.NodeID, "direction", "inbound")
	m.onPeerConnected(peer)
	peer.Run()
	m.removePeer(peer.NodeID)
	tlog.Info("peer disconnected", "peer", peer.NodeID)
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
			tlog.Debug("dial failed", "addr", addr, "err", err)
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
		_ = conn.Close()
		return fmt.Errorf("noise handshake: %w", err)
	}

	peer := newPeer(nc, peerX25519, m.id, m.hub, true)
	m.applyPeerTimeouts(peer)
	if err := peer.ExchangeHello(m.helloConfig()); err != nil {
		_ = nc.Close()
		return fmt.Errorf("hello exchange: %w", err)
	}

	if reason := m.addPeer(peer); reason != "" {
		_ = nc.Close()
		return fmt.Errorf("peer %s rejected: %s", peer.NodeID, reason)
	}

	tlog.Info("peer connected", "peer", peer.NodeID, "direction", "outbound")
	m.onPeerConnected(peer)
	peer.Run()
	m.removePeer(peer.NodeID)
	tlog.Info("peer disconnected", "peer", peer.NodeID)
	return nil
}

// fanoutLoop reads local user messages from the partyline and broadcasts them
// via the gossip engine. Each message becomes a single signed Envelope sent to
// all connected peers (first hop); intermediate nodes forward to a random subset.
func (m *Manager) fanoutLoop(ctx context.Context) {
	for {
		select {
		case msg := <-m.remoteSend:
			chat := &pb.ChatMessage{
				Nick:      msg.Nick,
				Text:      msg.Text,
				Timestamp: uint64(time.Now().Unix()),
			}
			payload, err := proto.Marshal(chat)
			if err != nil {
				tlog.Error("marshal chat", "err", err)
				continue
			}

			env := &pb.Envelope{
				MessageId:    uuid.New().String(),
				SenderId:     m.id.NodeID,
				MsgType:      MsgTypeChat,
				HopCount:     m.gossip.MaxHops(),
				Payload:      payload,
				SenderPubkey: m.id.PublicKey,
			}
			pb.SignEnvelope(env, m.id.PrivateKey)
			m.gossip.Broadcast(env)

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
	p.onGossipMessage = m.gossipMessageHandler()
	p.onStateSyncRequest = m.stateSyncRequestHandler()
	p.onStateSyncResponse = m.stateSyncResponseHandler()
	m.peers[p.NodeID] = p
	return ""
}

func (m *Manager) removePeer(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.peers, nodeID)
}

// peerHandles returns gossip PeerHandles for all currently connected peers.
// Called by the gossip engine when it needs to forward messages.
func (m *Manager) peerHandles() []gossip.PeerHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	handles := make([]gossip.PeerHandle, 0, len(m.peers))
	for _, p := range m.peers {
		peer := p // capture for closure
		handles = append(handles, gossip.PeerHandle{
			NodeID: peer.NodeID,
			Send:   peer.SendRaw,
		})
	}
	return handles
}

// gossipMessageHandler returns the callback set on each peer to route
// gossip-eligible messages to the engine.
func (m *Manager) gossipMessageHandler() func(string, *pb.Envelope) {
	return func(fromPeerID string, env *pb.Envelope) {
		m.gossip.HandleIncoming(fromPeerID, env)
	}
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

// SetPeerTimeouts configures the idle timeout and keepalive interval for all
// subsequently created peers. Must be called before Start().
//
// The idle parameter is the read-deadline timeout: if no data arrives within
// this window, the peer is disconnected. The keepalive parameter is the
// interval at which keepalive messages are sent on an otherwise idle connection.
//
// Passing 0 for either parameter leaves that timeout at the peer's default
// value. For the keepalive mechanism to be effective, keepalive should be less
// than idle so that keepalive messages are sent before the connection would be
// considered idle.
func (m *Manager) SetPeerTimeouts(idle, keepalive time.Duration) {
	if idle < 0 {
		idle = 0
	}
	if keepalive < 0 {
		keepalive = 0
	}
	m.mu.Lock()
	m.peerIdleTimeout = idle
	m.peerKeepaliveInterval = keepalive
	m.mu.Unlock()
}

// SetPeerWriteTimeout sets the write deadline timeout for all current and
// future peers. Intended for testing write timeout behavior.
func (m *Manager) SetPeerWriteTimeout(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peerWriteTimeout = d
	for _, p := range m.peers {
		p.writeTimeout.Store(int64(d))
	}
}

// applyPeerTimeouts overrides the peer's default timeouts if the manager
// has non-zero overrides configured.
func (m *Manager) applyPeerTimeouts(p *Peer) {
	m.mu.Lock()
	idle := m.peerIdleTimeout
	keepalive := m.peerKeepaliveInterval
	writeTimeout := m.peerWriteTimeout
	m.mu.Unlock()

	if idle > 0 {
		p.idleTimeout = idle
	}
	if keepalive > 0 {
		p.keepaliveInterval = keepalive
	}
	if writeTimeout > 0 {
		p.writeTimeout.Store(int64(writeTimeout))
	}
}

// enqueuePersist sends an entry to the persist worker. If the buffer is full
// the entry is dropped with a warning — bbolt serializes writes internally,
// so unbounded goroutine spawning is worse than occasionally losing a persist
// (the entry is still in memory and will be re-persisted on the next update).
func (m *Manager) enqueuePersist(entry state.Entry) {
	select {
	case m.persistCh <- entry:
	default:
		tlog.Warn("persist buffer full, dropping write", "key", entry.Key)
	}
}

// persistLoop drains the persist channel and writes entries to the store.
// Runs as a single goroutine to bound concurrency on bbolt writes.
func (m *Manager) persistLoop() {
	for {
		select {
		case entry := <-m.persistCh:
			state.PersistEntry(m.store, entry)
		case <-m.done:
			// Drain remaining entries before exiting.
			for {
				select {
				case entry := <-m.persistCh:
					state.PersistEntry(m.store, entry)
				default:
					return
				}
			}
		}
	}
}

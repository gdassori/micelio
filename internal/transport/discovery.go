package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"micelio/internal/gossip"
	"micelio/internal/logging"
	pb "micelio/pkg/proto"
)

var dlog = logging.For("discovery")

var peersBucket = []byte("peers")

// PeerRecord represents a known peer stored in the discovery table.
type PeerRecord struct {
	NodeID    string `json:"node_id"`
	Addr      string `json:"addr"`
	Reachable bool   `json:"reachable"`
	PublicKey string `json:"public_key"` // hex-encoded ED25519
	LastSeen  int64  `json:"last_seen"`  // unix seconds
	FailCount int    `json:"fail_count"`
	LastFail  int64  `json:"last_fail"` // unix seconds
}

// backoff returns the minimum time before this peer should be retried.
func (r *PeerRecord) backoff() time.Duration {
	if r.FailCount <= 0 {
		return 0
	}
	secs := math.Min(math.Pow(2, float64(r.FailCount-1)), 30)
	return time.Duration(secs) * time.Second
}

const (
	maxDialsPerTick = 3
	maxDialJitter   = 2 * time.Second
)

// loadKnownPeers reads all peer records from the store into knownPeers.
// Called once at startup. If store is nil, this is a no-op.
func (m *Manager) loadKnownPeers() {
	if m.store == nil {
		return
	}
	snap, err := m.store.Snapshot(peersBucket)
	if err != nil {
		dlog.Warn("loading known peers", "err", err)
		return
	}
	// Unmarshal and validate outside the lock
	var valid []*PeerRecord
	for _, v := range snap {
		var rec PeerRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			dlog.Warn("unmarshal peer record", "err", err)
			continue
		}
		if rec.NodeID == "" {
			dlog.Warn("skipping peer: empty NodeID")
			continue
		}
		if rec.Addr != "" && isInvalidPeerAddr(rec.Addr) {
			dlog.Warn("skipping peer: invalid addr", "peer", formatNodeIDShort(rec.NodeID), "addr", rec.Addr)
			continue
		}
		// Validate public key if present: must be valid hex, correct length, and match node_id
		if rec.PublicKey != "" {
			pubBytes, err := hex.DecodeString(rec.PublicKey)
			if err != nil {
				dlog.Warn("skipping peer: invalid pubkey encoding", "peer", formatNodeIDShort(rec.NodeID))
				continue
			}
			if len(pubBytes) != ed25519.PublicKeySize {
				dlog.Warn("skipping peer: invalid pubkey length", "peer", formatNodeIDShort(rec.NodeID))
				continue
			}
			hash := sha256.Sum256(pubBytes)
			if hex.EncodeToString(hash[:]) != rec.NodeID {
				dlog.Warn("skipping peer: mismatched pubkey", "peer", formatNodeIDShort(rec.NodeID))
				continue
			}
		}
		valid = append(valid, &rec)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range valid {
		m.knownPeers[rec.NodeID] = rec
	}
	if len(m.knownPeers) > 0 {
		dlog.Info("loaded known peers", "count", len(m.knownPeers))
	}
}

// saveKnownPeer persists a single peer record to the store.
func (m *Manager) saveKnownPeer(rec *PeerRecord) {
	if m.store == nil {
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		dlog.Error("marshal peer record", "err", err)
		return
	}
	if err := m.store.Set(peersBucket, []byte(rec.NodeID), data); err != nil {
		dlog.Error("save peer", "peer", rec.NodeID, "err", err)
	}
}

// onPeerConnected is called after a peer completes the hello exchange and is
// added to the peers map. It updates discovery state, adds the peer's key to
// the gossip keyring (TrustDirectlyVerified), and sends an initial PeerExchange.
func (m *Manager) onPeerConnected(p *Peer) {
	// Add peer's key to the gossip keyring with the highest trust level.
	m.gossip.KeyRing.Add(p.NodeID, p.edPubKey, gossip.TrustDirectlyVerified)

	m.mu.Lock()
	rec, ok := m.knownPeers[p.NodeID]
	if !ok {
		rec = &PeerRecord{NodeID: p.NodeID}
		m.knownPeers[p.NodeID] = rec
	}
	rec.Addr = p.remoteListenAddr
	rec.Reachable = p.remoteReachable
	rec.PublicKey = hex.EncodeToString(p.edPubKey)
	rec.LastSeen = time.Now().Unix()
	rec.FailCount = 0
	rec.LastFail = 0
	snapshot := *rec // copy before releasing lock
	m.mu.Unlock()

	go m.saveKnownPeer(&snapshot)
	m.sendPeerExchangeTo(p)
	m.sendStateSyncRequestTo(p)
}

// exchangeLoop periodically sends PeerExchange to all connected peers.
func (m *Manager) exchangeLoop(ctx context.Context) {
	interval := m.cfg.Network.ExchangeInterval.Duration
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			peers := make([]*Peer, 0, len(m.peers))
			for _, p := range m.peers {
				peers = append(peers, p)
			}
			m.mu.Unlock()
			for _, p := range peers {
				m.sendPeerExchangeTo(p)
			}
		case <-ctx.Done():
			return
		case <-m.done:
			return
		}
	}
}

// discoveryLoop periodically scans knownPeers and dials candidates.
func (m *Manager) discoveryLoop(ctx context.Context) {
	interval := m.cfg.Network.DiscoveryInterval.Duration
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.tryDiscoveryDials(ctx)
		case <-ctx.Done():
			return
		case <-m.done:
			return
		}
	}
}

// sendPeerExchangeTo builds and sends a PeerExchange message to a single peer.
func (m *Manager) sendPeerExchangeTo(p *Peer) {
	infos := m.buildPeerInfoList(p.NodeID)
	if len(infos) == 0 {
		return
	}

	px := &pb.PeerExchange{Peers: infos}
	payload, err := proto.Marshal(px)
	if err != nil {
		dlog.Error("marshal peer_exchange", "err", err)
		return
	}

	env := &pb.Envelope{
		MessageId: uuid.New().String(),
		SenderId:  m.id.NodeID,
		MsgType:   MsgTypePeerExchange,
		HopCount:  1,
		Payload:   payload,
	}
	signEnvelope(env, m.id.PrivateKey)

	envBytes, err := proto.Marshal(env)
	if err != nil {
		dlog.Error("marshal envelope", "err", err)
		return
	}

	select {
	case p.sendCh <- envBytes:
	default:
		dlog.Warn("send buffer full, dropping exchange", "peer", p.NodeID)
	}
}

// buildPeerInfoList collects reachable peers for exchange, excluding the
// recipient and self (self is added separately if reachable).
func (m *Manager) buildPeerInfoList(excludeNodeID string) []*pb.PeerInfo {
	selfAddr := m.advertiseAddr()

	// Snapshot minimal fields under lock to avoid holding m.mu during hex decode
	type peerSnapshot struct {
		nodeID       string
		addr         string
		reachable    bool
		publicKeyHex string
		lastSeen     int64
	}

	m.mu.Lock()
	snapshots := make([]peerSnapshot, 0, len(m.knownPeers))
	for _, rec := range m.knownPeers {
		if rec.NodeID == excludeNodeID || rec.NodeID == m.id.NodeID {
			continue
		}
		if !rec.Reachable || rec.Addr == "" || isInvalidPeerAddr(rec.Addr) {
			continue
		}
		snapshots = append(snapshots, peerSnapshot{
			nodeID:       rec.NodeID,
			addr:         rec.Addr,
			reachable:    rec.Reachable,
			publicKeyHex: rec.PublicKey,
			lastSeen:     rec.LastSeen,
		})
	}
	m.mu.Unlock()

	var infos []*pb.PeerInfo

	// Add self if reachable
	if selfAddr != "" {
		infos = append(infos, &pb.PeerInfo{
			NodeId:        m.id.NodeID,
			Addr:          selfAddr,
			Reachable:     true,
			Ed25519Pubkey: m.id.PublicKey,
			LastSeen:      uint64(time.Now().Unix()),
		})
	}

	// Add known reachable peers (decode hex outside lock)
	for _, s := range snapshots {
		if s.publicKeyHex == "" {
			continue
		}
		pubKey, err := hex.DecodeString(s.publicKeyHex)
		if err != nil || len(pubKey) != ed25519.PublicKeySize {
			continue
		}
		infos = append(infos, &pb.PeerInfo{
			NodeId:        s.nodeID,
			Addr:          s.addr,
			Reachable:     s.reachable,
			Ed25519Pubkey: pubKey,
			LastSeen:      uint64(s.lastSeen),
		})
	}

	return infos
}

// isInvalidPeerAddr returns true if addr is not a valid dialable host:port.
// Rejects wildcard hosts (0.0.0.0, ::, empty) and malformed addresses.
func isInvalidPeerAddr(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return true // malformed, not a valid host:port
	}
	if port == "" {
		return true
	}
	return host == "" || host == "0.0.0.0" || host == "::"
}

// safeLastSeen converts a uint64 timestamp to int64, clamping at MaxInt64.
func safeLastSeen(ts uint64) int64 {
	if ts > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(ts)
}

// mergeDiscoveredPeers merges received PeerInfo into knownPeers and persists.
func (m *Manager) mergeDiscoveredPeers(infos []*pb.PeerInfo, fromNodeID string) {
	// Pre-filter and validate outside the lock (sha256, hex encoding, addr checks)
	type validEntry struct {
		info      *pb.PeerInfo
		pubKeyHex string
		lastSeen  int64
	}
	var validated []validEntry
	for _, info := range infos {
		if info.NodeId == m.id.NodeID {
			continue
		}
		if !info.Reachable || info.Addr == "" || isInvalidPeerAddr(info.Addr) {
			continue
		}
		if len(info.Ed25519Pubkey) != ed25519.PublicKeySize {
			continue
		}
		hash := sha256.Sum256(info.Ed25519Pubkey)
		if info.NodeId != hex.EncodeToString(hash[:]) {
			dlog.Warn("ignoring peer: mismatched node_id", "from", formatNodeIDShort(fromNodeID))
			continue
		}
		// Add to gossip keyring as gossip-learned (won't downgrade direct trust).
		m.gossip.KeyRing.Add(info.NodeId, info.Ed25519Pubkey, gossip.TrustGossipLearned)

		validated = append(validated, validEntry{
			info:      info,
			pubKeyHex: hex.EncodeToString(info.Ed25519Pubkey),
			lastSeen:  safeLastSeen(info.LastSeen),
		})
	}

	if len(validated) == 0 {
		return
	}

	// Lock only to apply updates to knownPeers
	m.mu.Lock()
	var toPersist []PeerRecord
	for _, v := range validated {
		existing, ok := m.knownPeers[v.info.NodeId]
		if ok {
			if v.lastSeen > existing.LastSeen {
				existing.Addr = v.info.Addr
				existing.Reachable = v.info.Reachable
				existing.PublicKey = v.pubKeyHex
				existing.LastSeen = v.lastSeen
				toPersist = append(toPersist, *existing)
			}
		} else {
			rec := &PeerRecord{
				NodeID:    v.info.NodeId,
				Addr:      v.info.Addr,
				Reachable: v.info.Reachable,
				PublicKey:  v.pubKeyHex,
				LastSeen:  v.lastSeen,
			}
			m.knownPeers[v.info.NodeId] = rec
			toPersist = append(toPersist, *rec)
		}
	}
	m.mu.Unlock()

	// Persist outside the lock and off the recv goroutine to avoid stalls
	if len(toPersist) > 0 {
		go func(recs []PeerRecord) {
			for i := range recs {
				m.saveKnownPeer(&recs[i])
			}
		}(toPersist)
	}
}

// tryDiscoveryDials selects candidates from knownPeers and dials them.
func (m *Manager) tryDiscoveryDials(ctx context.Context) {
	m.mu.Lock()

	// Don't dial if at capacity
	if m.cfg.Network.MaxPeers > 0 && len(m.peers) >= m.cfg.Network.MaxPeers {
		m.mu.Unlock()
		return
	}

	now := time.Now()
	var candidates []*PeerRecord

	for _, rec := range m.knownPeers {
		// Skip self
		if rec.NodeID == m.id.NodeID {
			continue
		}
		// Must be reachable with an address
		if !rec.Reachable || rec.Addr == "" {
			continue
		}
		// Already connected
		if _, connected := m.peers[rec.NodeID]; connected {
			continue
		}
		// Already dialing
		if m.dialing[rec.NodeID] {
			continue
		}
		// Managed by bootstrap dialWithBackoff — skip to avoid double-dial
		if m.bootstrapAddrs[rec.Addr] {
			continue
		}
		// Backoff not elapsed
		if rec.FailCount > 0 {
			backoffEnd := time.Unix(rec.LastFail, 0).Add(rec.backoff())
			if now.Before(backoffEnd) {
				continue
			}
		}
		// Copy record to avoid races with concurrent writes to the pointer
		copied := *rec
		candidates = append(candidates, &copied)
	}
	m.mu.Unlock()

	if len(candidates) == 0 {
		return
	}

	// Shuffle candidates
	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})

	// Cap at maxDialsPerTick
	if len(candidates) > maxDialsPerTick {
		candidates = candidates[:maxDialsPerTick]
	}

	for _, rec := range candidates {
		m.mu.Lock()
		m.dialing[rec.NodeID] = true
		m.mu.Unlock()
		go m.dialDiscovered(ctx, rec)
	}
}

// dialDiscovered dials a discovered peer with random jitter.
func (m *Manager) dialDiscovered(ctx context.Context, rec *PeerRecord) {
	defer func() {
		m.mu.Lock()
		delete(m.dialing, rec.NodeID)
		m.mu.Unlock()
	}()

	// Random jitter 0-2s
	jitter := time.Duration(rand.Int64N(int64(maxDialJitter)))
	timer := time.NewTimer(jitter)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	case <-m.done:
		return
	}

	if err := m.dial(rec.Addr); err != nil {
		var snapshot *PeerRecord
		m.mu.Lock()
		if r, ok := m.knownPeers[rec.NodeID]; ok {
			r.FailCount++
			r.LastFail = time.Now().Unix()
			s := *r
			snapshot = &s
		}
		m.mu.Unlock()
		if snapshot != nil {
			m.saveKnownPeer(snapshot)
		}
		dlog.Debug("dial discovered peer failed", "peer", formatNodeIDShort(rec.NodeID), "addr", rec.Addr, "err", err)
	}
}

// formatNodeIDShort returns first 12 chars of a node ID for logging.
func formatNodeIDShort(nodeID string) string {
	if len(nodeID) > 12 {
		return nodeID[:12]
	}
	return nodeID
}

// peerExchangeHandler returns the callback function that the manager sets on peers
// to handle incoming PeerExchange messages.
func (m *Manager) peerExchangeHandler() func(string, []*pb.PeerInfo) {
	return func(nodeID string, infos []*pb.PeerInfo) {
		m.mergeDiscoveredPeers(infos, nodeID)
		if count := len(infos); count > 0 {
			dlog.Debug("received peer exchange", "count", count, "from", formatNodeIDShort(nodeID))
		}
	}
}

func (r *PeerRecord) String() string {
	return fmt.Sprintf("PeerRecord{%s addr=%s reachable=%v fails=%d}", formatNodeIDShort(r.NodeID), r.Addr, r.Reachable, r.FailCount)
}

package transport

import (
	"sync"
	"time"

	"micelio/internal/logging"
	"micelio/internal/state"
	pb "micelio/pkg/proto"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	// stateSyncRequestCooldown is the minimum interval between StateSyncRequests
	// from the same peer. Prevents amplification attacks where a malicious peer
	// repeatedly requests a full state dump.
	stateSyncRequestCooldown = 30 * time.Second

	// maxStateSyncEntries is the maximum number of entries sent in a single
	// StateSyncResponse. Prevents OOM from serializing a very large state map
	// and keeps frame sizes bounded.
	maxStateSyncEntries = 1000
)

var sslog = logging.For("state-sync")

// sendStateSyncRequestTo sends a StateSyncRequest to a single peer.
func (m *Manager) sendStateSyncRequestTo(p *Peer) {
	if m.stateMap == nil {
		return
	}

	digest := m.stateMap.Digest()
	payload, err := proto.Marshal(&pb.StateSyncRequest{Known: digest})
	if err != nil {
		sslog.Error("marshal state_sync_request", "err", err)
		return
	}

	env := &pb.Envelope{
		MessageId: uuid.New().String(),
		SenderId:  m.id.NodeID,
		MsgType:   MsgTypeStateSyncRequest,
		HopCount:  1,
		Payload:   payload,
	}
	signEnvelope(env, m.id.PrivateKey)

	envBytes, err := proto.Marshal(env)
	if err != nil {
		sslog.Error("marshal state_sync_request envelope", "err", err)
		return
	}

	select {
	case p.sendCh <- envBytes:
		sslog.Debug("sent state_sync_request", "peer", formatNodeIDShort(p.NodeID), "digest_nodes", len(digest))
	default:
		sslog.Warn("send buffer full, dropping state_sync_request", "peer", formatNodeIDShort(p.NodeID))
	}
}

// sendStateSyncResponseTo sends state entries the peer is missing, based on
// the version vector digest it provided. If known is nil/empty, all entries
// are sent (backward compatible with fresh nodes).
func (m *Manager) sendStateSyncResponseTo(p *Peer, known map[string]uint64) {
	if m.stateMap == nil {
		return
	}

	delta := m.stateMap.SnapshotDelta(known)
	if len(delta) == 0 {
		sslog.Debug("skipping empty state_sync_response", "peer", formatNodeIDShort(p.NodeID))
		return
	}

	// Cap entries to prevent oversized responses.
	if len(delta) > maxStateSyncEntries {
		sslog.Warn("truncating state_sync_response",
			"peer", formatNodeIDShort(p.NodeID),
			"total", len(delta),
			"sending", maxStateSyncEntries)
		delta = delta[:maxStateSyncEntries]
	}

	pbEntries := make([]*pb.StateEntry, len(delta))
	for i, e := range delta {
		pbEntries[i] = &pb.StateEntry{
			Key:          e.Key,
			Value:        e.Value,
			LamportTs:    e.LamportTs,
			NodeId:       e.NodeID,
			Deleted:      e.Deleted,
			Signature:    e.Signature,
			AuthorPubkey: e.AuthorPubkey,
		}
	}

	payload, err := proto.Marshal(&pb.StateSyncResponse{Entries: pbEntries})
	if err != nil {
		sslog.Error("marshal state_sync_response", "err", err)
		return
	}

	env := &pb.Envelope{
		MessageId: uuid.New().String(),
		SenderId:  m.id.NodeID,
		MsgType:   MsgTypeStateSyncResponse,
		HopCount:  1,
		Payload:   payload,
	}
	signEnvelope(env, m.id.PrivateKey)

	envBytes, err := proto.Marshal(env)
	if err != nil {
		sslog.Error("marshal state_sync_response envelope", "err", err)
		return
	}

	select {
	case p.sendCh <- envBytes:
		sslog.Debug("sent state_sync_response", "peer", formatNodeIDShort(p.NodeID), "entries", len(delta))
	default:
		sslog.Warn("send buffer full, dropping state_sync_response", "peer", formatNodeIDShort(p.NodeID))
	}
}

// stateSyncRequestHandler returns the callback for incoming StateSyncRequest.
// It enforces a per-peer cooldown to prevent amplification attacks.
func (m *Manager) stateSyncRequestHandler() func(fromPeerID string, known map[string]uint64) {
	var (
		mu       sync.Mutex
		lastSync = make(map[string]time.Time)
	)
	return func(fromPeerID string, known map[string]uint64) {
		now := time.Now()
		mu.Lock()
		if last, ok := lastSync[fromPeerID]; ok && now.Sub(last) < stateSyncRequestCooldown {
			mu.Unlock()
			sslog.Warn("rate-limited state_sync_request", "from", formatNodeIDShort(fromPeerID))
			return
		}
		lastSync[fromPeerID] = now
		mu.Unlock()

		sslog.Debug("received state_sync_request", "from", formatNodeIDShort(fromPeerID), "digest_nodes", len(known))
		m.mu.Lock()
		p, ok := m.peers[fromPeerID]
		m.mu.Unlock()
		if ok {
			go m.sendStateSyncResponseTo(p, known)
		}
	}
}

// stateSyncResponseHandler returns the callback for incoming StateSyncResponse.
func (m *Manager) stateSyncResponseHandler() func(fromPeerID string, entries []*pb.StateEntry) {
	return func(fromPeerID string, entries []*pb.StateEntry) {
		sslog.Debug("received state_sync_response", "from", formatNodeIDShort(fromPeerID), "entries", len(entries))
		for _, pbEntry := range entries {
			entry := state.Entry{
				Key:          pbEntry.Key,
				Value:        pbEntry.Value,
				LamportTs:    pbEntry.LamportTs,
				NodeID:       pbEntry.NodeId,
				Deleted:      pbEntry.Deleted,
				Signature:    pbEntry.Signature,
				AuthorPubkey: pbEntry.AuthorPubkey,
			}
			if m.stateMap.Merge(entry) {
				m.enqueuePersist(entry)
			}
		}
	}
}

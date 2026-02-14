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

	payload, err := proto.Marshal(&pb.StateSyncRequest{})
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
		sslog.Debug("sent state_sync_request", "peer", formatNodeIDShort(p.NodeID))
	default:
		sslog.Warn("send buffer full, dropping state_sync_request", "peer", formatNodeIDShort(p.NodeID))
	}
}

// sendStateSyncResponseTo sends a full state snapshot to a single peer.
func (m *Manager) sendStateSyncResponseTo(p *Peer) {
	if m.stateMap == nil {
		return
	}

	snapshot := m.stateMap.Snapshot()
	if len(snapshot) == 0 {
		sslog.Debug("skipping empty state_sync_response", "peer", formatNodeIDShort(p.NodeID))
		return
	}

	// Cap entries to prevent oversized responses.
	if len(snapshot) > maxStateSyncEntries {
		sslog.Warn("truncating state_sync_response",
			"peer", formatNodeIDShort(p.NodeID),
			"total", len(snapshot),
			"sending", maxStateSyncEntries)
		snapshot = snapshot[:maxStateSyncEntries]
	}

	pbEntries := make([]*pb.StateEntry, len(snapshot))
	for i, e := range snapshot {
		pbEntries[i] = &pb.StateEntry{
			Key:       e.Key,
			Value:     e.Value,
			LamportTs: e.LamportTs,
			NodeId:    e.NodeID,
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
		sslog.Debug("sent state_sync_response", "peer", formatNodeIDShort(p.NodeID), "entries", len(snapshot))
	default:
		sslog.Warn("send buffer full, dropping state_sync_response", "peer", formatNodeIDShort(p.NodeID))
	}
}

// stateSyncRequestHandler returns the callback for incoming StateSyncRequest.
// It enforces a per-peer cooldown to prevent amplification attacks.
func (m *Manager) stateSyncRequestHandler() func(fromPeerID string) {
	var (
		mu       sync.Mutex
		lastSync = make(map[string]time.Time)
	)
	return func(fromPeerID string) {
		now := time.Now()
		mu.Lock()
		if last, ok := lastSync[fromPeerID]; ok && now.Sub(last) < stateSyncRequestCooldown {
			mu.Unlock()
			sslog.Warn("rate-limited state_sync_request", "from", formatNodeIDShort(fromPeerID))
			return
		}
		lastSync[fromPeerID] = now
		mu.Unlock()

		sslog.Debug("received state_sync_request", "from", formatNodeIDShort(fromPeerID))
		m.mu.Lock()
		p, ok := m.peers[fromPeerID]
		m.mu.Unlock()
		if ok {
			go m.sendStateSyncResponseTo(p)
		}
	}
}

// stateSyncResponseHandler returns the callback for incoming StateSyncResponse.
func (m *Manager) stateSyncResponseHandler() func(fromPeerID string, entries []*pb.StateEntry) {
	return func(fromPeerID string, entries []*pb.StateEntry) {
		sslog.Debug("received state_sync_response", "from", formatNodeIDShort(fromPeerID), "entries", len(entries))
		for _, pbEntry := range entries {
			entry := state.Entry{
				Key:       pbEntry.Key,
				Value:     pbEntry.Value,
				LamportTs: pbEntry.LamportTs,
				NodeID:    pbEntry.NodeId,
			}
			if m.stateMap.Merge(entry) {
				m.enqueuePersist(entry)
			}
		}
	}
}

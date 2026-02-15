package state

import (
	"micelio/internal/store"
	pb "micelio/pkg/proto"

	"google.golang.org/protobuf/proto"
)

var stateBucket = []byte("state")

// LoadFromStore loads persisted state entries into the map and
// sets the Lamport clock floor to the highest persisted timestamp.
// Tombstones are loaded and tracked for GC with a fresh first-seen time.
// Called once at startup. If st is nil, this is a no-op.
func (m *Map) LoadFromStore(st store.Store) error {
	if st == nil {
		return nil
	}

	var maxTs uint64
	err := st.ForEach(stateBucket, func(key, value []byte) error {
		var pbEntry pb.StateEntry
		if err := proto.Unmarshal(value, &pbEntry); err != nil {
			logger.Warn("skipping corrupt state entry", "key", string(key), "err", err)
			return nil
		}
		entry := Entry{
			Key:          pbEntry.Key,
			Value:        pbEntry.Value,
			LamportTs:    pbEntry.LamportTs,
			NodeID:       pbEntry.NodeId,
			Deleted:      pbEntry.Deleted,
			Signature:    pbEntry.Signature,
			AuthorPubkey: pbEntry.AuthorPubkey,
		}
		m.mu.Lock()
		m.entries[entry.Key] = entry
		m.mu.Unlock()
		if entry.Deleted {
			m.RecordTombstone(entry.Key)
		}
		if entry.LamportTs > maxTs {
			maxTs = entry.LamportTs
		}
		return nil
	})
	if err != nil {
		return err
	}

	if maxTs > 0 {
		m.clock.SetFloor(maxTs)
		logger.Info("loaded state from store", "entries", m.Len(), "max_ts", maxTs)
	}
	return nil
}

// PersistEntry writes a single entry to the store.
// If st is nil, this is a no-op.
func PersistEntry(st store.Store, entry Entry) {
	if st == nil {
		return
	}
	pbEntry := &pb.StateEntry{
		Key:          entry.Key,
		Value:        entry.Value,
		LamportTs:    entry.LamportTs,
		NodeId:       entry.NodeID,
		Deleted:      entry.Deleted,
		Signature:    entry.Signature,
		AuthorPubkey: entry.AuthorPubkey,
	}
	data, err := proto.Marshal(pbEntry)
	if err != nil {
		logger.Error("marshal state entry for persistence", "key", entry.Key, "err", err)
		return
	}
	if err := st.Set(stateBucket, []byte(entry.Key), data); err != nil {
		logger.Error("persist state entry", "key", entry.Key, "err", err)
	}
}

// DeleteEntry removes an entry from the store by key.
// Used by GC to clean up tombstones from disk.
// If st is nil, this is a no-op.
func DeleteEntry(st store.Store, key string) {
	if st == nil {
		return
	}
	if err := st.Delete(stateBucket, []byte(key)); err != nil {
		logger.Error("delete state entry from store", "key", key, "err", err)
	}
}

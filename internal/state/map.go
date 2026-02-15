package state

import (
	"crypto/ed25519"
	"errors"
	"sync"
	"time"

	"micelio/internal/logging"
)

// Limits to prevent resource exhaustion from malicious peers.
const (
	MaxKeyLen        = 256      // max key length in bytes
	MaxValueLen      = 4096     // max value length in bytes
	MaxStateEntries  = 10_000   // max entries in the state map
	MaxSafeLamportTs = 1 << 62 // reject timestamps above this to prevent clock poisoning
)

var (
	ErrKeyTooLong   = errors.New("key exceeds max length")
	ErrValueTooLong = errors.New("value exceeds max length")
	ErrMapFull      = errors.New("state map at capacity")
	ErrTsOverflow   = errors.New("lamport timestamp exceeds safe maximum")
)

var logger = logging.For("state")

// Entry is a single state entry with LWW metadata.
// Each entry is self-authenticating: it carries the author's public key
// and a signature. Nodes verify before accepting remote entries.
type Entry struct {
	Key          string
	Value        []byte
	LamportTs    uint64
	NodeID       string
	Deleted      bool   // tombstone marker
	Signature    []byte // ED25519 signature over semantic fields
	AuthorPubkey []byte // 32-byte ED25519 public key of author
}

// Wins returns true if this entry wins over other according to LWW rules:
// higher lamport_ts wins; on tie, lexicographically greater node_id wins.
func (e Entry) Wins(other Entry) bool {
	if e.LamportTs != other.LamportTs {
		return e.LamportTs > other.LamportTs
	}
	return e.NodeID > other.NodeID
}

// ChangeHandler is called when a local state entry is accepted (wins LWW).
// Used by the transport layer to broadcast state updates via gossip.
type ChangeHandler func(entry Entry)

// Map is a thread-safe in-memory LWW state map.
type Map struct {
	mu                 sync.RWMutex
	entries            map[string]Entry
	tombstoneFirstSeen map[string]time.Time // key → time tombstone was first seen locally
	clock              *LamportClock
	localID            string
	privKey            ed25519.PrivateKey
	onChange           ChangeHandler
}

// NewMap creates a new state map for the given local node ID.
// The private key is used to sign entries created by Set/Delete.
func NewMap(localID string, privKey ed25519.PrivateKey) *Map {
	return &Map{
		entries:            make(map[string]Entry),
		tombstoneFirstSeen: make(map[string]time.Time),
		clock:              &LamportClock{},
		localID:            localID,
		privKey:            privKey,
	}
}

// SetChangeHandler sets the callback invoked when a local Set/Delete wins LWW.
// Must be called before any concurrent access.
func (m *Map) SetChangeHandler(h ChangeHandler) {
	m.onChange = h
}

// Clock returns the map's Lamport clock.
func (m *Map) Clock() *LamportClock {
	return m.clock
}

// Set creates or updates a local entry. The subkey is auto-prefixed with
// the local node ID to form the full key (<nodeID>/<subkey>).
// Ticks the Lamport clock, signs the entry, applies LWW, and if the
// write wins, calls the change handler.
// Returns an error if key or value exceeds size limits.
func (m *Map) Set(subkey string, value []byte) (Entry, bool, error) {
	fullKey := m.localID + "/" + subkey
	if len(fullKey) > MaxKeyLen {
		return Entry{}, false, ErrKeyTooLong
	}
	if len(value) > MaxValueLen {
		return Entry{}, false, ErrValueTooLong
	}
	ts := m.clock.Tick()
	entry := Entry{
		Key:       fullKey,
		Value:     value,
		LamportTs: ts,
		NodeID:    m.localID,
	}
	SignEntry(&entry, m.privKey)
	return entry, m.merge(entry, true), nil
}

// Delete creates a tombstone for the given subkey. The subkey is auto-prefixed
// with the local node ID to form the full key (<nodeID>/<subkey>).
// Ticks the Lamport clock, signs the entry, applies LWW, and if the
// delete wins, calls the change handler.
// Returns an error if key exceeds size limits.
func (m *Map) Delete(subkey string) (Entry, bool, error) {
	fullKey := m.localID + "/" + subkey
	if len(fullKey) > MaxKeyLen {
		return Entry{}, false, ErrKeyTooLong
	}
	ts := m.clock.Tick()
	entry := Entry{
		Key:       fullKey,
		LamportTs: ts,
		NodeID:    m.localID,
		Deleted:   true,
	}
	SignEntry(&entry, m.privKey)
	return entry, m.merge(entry, true), nil
}

// Merge applies a remote entry using LWW conflict resolution.
// Verifies the entry's signature and namespace ownership before accepting.
// Witnesses the entry's Lamport timestamp to advance the local clock.
// Returns true if the incoming entry won and was applied.
// Rejects entries that fail signature verification, exceed size limits,
// or have unsafe timestamps.
func (m *Map) Merge(entry Entry) bool {
	if !VerifyEntry(entry) {
		logger.Warn("rejecting entry with invalid signature or namespace",
			"key", entry.Key, "node_id", entry.NodeID)
		return false
	}
	if len(entry.Key) > MaxKeyLen || len(entry.Value) > MaxValueLen {
		logger.Warn("rejecting oversized entry", "key_len", len(entry.Key), "value_len", len(entry.Value))
		return false
	}
	if entry.LamportTs > MaxSafeLamportTs {
		logger.Warn("rejecting entry with unsafe timestamp", "ts", entry.LamportTs, "key", entry.Key)
		return false
	}
	m.clock.Witness(entry.LamportTs)
	return m.merge(entry, false)
}

func (m *Map) merge(entry Entry, notify bool) bool {
	m.mu.Lock()
	existing, exists := m.entries[entry.Key]
	if exists && !entry.Wins(existing) {
		m.mu.Unlock()
		return false
	}
	// Reject new keys when at capacity (updates to existing keys are always allowed).
	if !exists && len(m.entries) >= MaxStateEntries {
		m.mu.Unlock()
		logger.Warn("state map at capacity, rejecting new key", "key", entry.Key)
		return false
	}
	m.entries[entry.Key] = entry

	// Track tombstone first-seen time for GC.
	if entry.Deleted {
		if _, tracked := m.tombstoneFirstSeen[entry.Key]; !tracked {
			m.tombstoneFirstSeen[entry.Key] = time.Now()
		}
	} else {
		// A live entry overwrites a tombstone — stop tracking.
		delete(m.tombstoneFirstSeen, entry.Key)
	}
	m.mu.Unlock()

	if notify && m.onChange != nil {
		m.onChange(entry)
	}
	return true
}

// Get returns the entry for a key, or false if not found or deleted.
func (m *Map) Get(key string) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[key]
	if !ok || e.Deleted {
		return Entry{}, false
	}
	return e, true
}

// Snapshot returns a copy of all entries, including tombstones.
// Tombstones must be included for state sync to propagate deletes.
func (m *Map) Snapshot() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entries := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	return entries
}

// Digest returns a version vector summarizing the state map:
// for each nodeID, the maximum LamportTs of any entry from that node.
// Used by delta state sync to avoid sending entries the peer already has.
func (m *Map) Digest() map[string]uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d := make(map[string]uint64)
	for _, e := range m.entries {
		if e.LamportTs > d[e.NodeID] {
			d[e.NodeID] = e.LamportTs
		}
	}
	return d
}

// SnapshotDelta returns only entries the remote peer is missing, based on
// the version vector digest it provided. An entry is included if its
// LamportTs is strictly greater than known[entry.NodeID]. If known is
// nil or empty, all entries are returned (equivalent to Snapshot).
func (m *Map) SnapshotDelta(known map[string]uint64) []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var delta []Entry
	for _, e := range m.entries {
		if e.LamportTs > known[e.NodeID] {
			delta = append(delta, e)
		}
	}
	return delta
}

// Len returns the number of entries in the map, including tombstones.
func (m *Map) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// GC removes tombstones older than maxAge from the map.
// Returns the keys that were removed (caller should delete from store).
func (m *Map) GC(maxAge time.Duration) []string {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	var removed []string
	for key, firstSeen := range m.tombstoneFirstSeen {
		if now.Sub(firstSeen) >= maxAge {
			delete(m.entries, key)
			delete(m.tombstoneFirstSeen, key)
			removed = append(removed, key)
		}
	}
	if len(removed) > 0 {
		logger.Info("tombstone GC", "removed", len(removed))
	}
	return removed
}

// RecordTombstone records the first-seen time of a tombstone entry.
// Used by LoadFromStore to track tombstones loaded from disk.
func (m *Map) RecordTombstone(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, tracked := m.tombstoneFirstSeen[key]; !tracked {
		m.tombstoneFirstSeen[key] = time.Now()
	}
}

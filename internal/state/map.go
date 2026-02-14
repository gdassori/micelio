package state

import (
	"errors"
	"sync"

	"micelio/internal/logging"
)

// Limits to prevent resource exhaustion from malicious peers.
const (
	MaxKeyLen        = 256        // max key length in bytes
	MaxValueLen      = 4096       // max value length in bytes
	MaxStateEntries  = 10_000     // max entries in the state map
	MaxSafeLamportTs = 1 << 62   // reject timestamps above this to prevent clock poisoning
)

var (
	ErrKeyTooLong   = errors.New("key exceeds max length")
	ErrValueTooLong = errors.New("value exceeds max length")
	ErrMapFull      = errors.New("state map at capacity")
	ErrTsOverflow   = errors.New("lamport timestamp exceeds safe maximum")
)

var logger = logging.For("state")

// Entry is a single state entry with LWW metadata.
type Entry struct {
	Key       string
	Value     []byte
	LamportTs uint64
	NodeID    string
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
	mu       sync.RWMutex
	entries  map[string]Entry
	clock    *LamportClock
	localID  string
	onChange ChangeHandler
}

// NewMap creates a new state map for the given local node ID.
func NewMap(localID string) *Map {
	return &Map{
		entries: make(map[string]Entry),
		clock:   &LamportClock{},
		localID: localID,
	}
}

// SetChangeHandler sets the callback invoked when a local Set wins LWW.
// Must be called before any concurrent access.
func (m *Map) SetChangeHandler(h ChangeHandler) {
	m.onChange = h
}

// Clock returns the map's Lamport clock.
func (m *Map) Clock() *LamportClock {
	return m.clock
}

// Set creates or updates a local entry. Ticks the Lamport clock,
// applies LWW, and if the write wins, calls the change handler.
// Returns an error if key or value exceeds size limits.
func (m *Map) Set(key string, value []byte) (Entry, bool, error) {
	if len(key) > MaxKeyLen {
		return Entry{}, false, ErrKeyTooLong
	}
	if len(value) > MaxValueLen {
		return Entry{}, false, ErrValueTooLong
	}
	ts := m.clock.Tick()
	entry := Entry{
		Key:       key,
		Value:     value,
		LamportTs: ts,
		NodeID:    m.localID,
	}
	return entry, m.merge(entry, true), nil
}

// Merge applies a remote entry using LWW conflict resolution.
// Witnesses the entry's Lamport timestamp to advance the local clock.
// Returns true if the incoming entry won and was applied.
// Rejects entries that exceed size limits or have unsafe timestamps.
func (m *Map) Merge(entry Entry) bool {
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
	m.mu.Unlock()

	if notify && m.onChange != nil {
		m.onChange(entry)
	}
	return true
}

// Get returns the entry for a key, or false if not found.
func (m *Map) Get(key string) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[key]
	return e, ok
}

// Snapshot returns a copy of all entries.
func (m *Map) Snapshot() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entries := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	return entries
}

// Len returns the number of entries in the map.
func (m *Map) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

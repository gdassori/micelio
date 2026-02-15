package state

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// testID generates an ED25519 key pair and derives the nodeID (sha256 hex of pubkey).
func testID(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	hash := sha256.Sum256(pub)
	nodeID := hex.EncodeToString(hash[:])
	return nodeID, priv
}

// testMap creates a Map with a freshly generated identity.
func testMap(t *testing.T) (*Map, string) {
	t.Helper()
	nodeID, priv := testID(t)
	return NewMap(nodeID, priv), nodeID
}

// testSignedEntry creates a properly signed entry from the given identity.
func testSignedEntry(nodeID string, privKey ed25519.PrivateKey, subkey string, value []byte, ts uint64, deleted bool) Entry {
	e := Entry{
		Key:       nodeID + "/" + subkey,
		Value:     value,
		LamportTs: ts,
		NodeID:    nodeID,
		Deleted:   deleted,
	}
	SignEntry(&e, privKey)
	return e
}

func TestMapSetAndGet(t *testing.T) {
	m, nodeID := testMap(t)
	entry, ok, err := m.Set("color", []byte("blue"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Set rejected")
	}
	if entry.LamportTs != 1 {
		t.Fatalf("LamportTs = %d, want 1", entry.LamportTs)
	}

	fullKey := nodeID + "/color"
	got, found := m.Get(fullKey)
	if !found {
		t.Fatal("Get returned not found")
	}
	if string(got.Value) != "blue" {
		t.Fatalf("Value = %q, want %q", got.Value, "blue")
	}
	if got.NodeID != nodeID {
		t.Fatalf("NodeID = %q, want %q", got.NodeID, nodeID)
	}
}

func TestMapGetNotFound(t *testing.T) {
	m, nodeID := testMap(t)
	_, found := m.Get(nodeID + "/missing")
	if found {
		t.Fatal("expected not found")
	}
}

func TestMapLWWHigherTsWins(t *testing.T) {
	m, _ := testMap(t)
	m.Set("key", []byte("old"))

	remoteID, remotePriv := testID(t)
	remote := testSignedEntry(remoteID, remotePriv, "key", []byte("new"), 100, false)
	won := m.Merge(remote)
	if !won {
		t.Fatal("higher ts should win")
	}
	got, _ := m.Get(remote.Key)
	if string(got.Value) != "new" {
		t.Fatalf("Value = %q, want %q", got.Value, "new")
	}
}

func TestMapLWWLowerTsLoses(t *testing.T) {
	m, _ := testMap(t)
	remoteID, remotePriv := testID(t)

	// Set a value with high ts
	high := testSignedEntry(remoteID, remotePriv, "key", []byte("winner"), 100, false)
	m.Merge(high)

	// Try to merge a lower ts from the SAME node — should be rejected
	low := testSignedEntry(remoteID, remotePriv, "key", []byte("loser"), 50, false)
	won := m.Merge(low)
	if won {
		t.Fatal("lower ts should lose")
	}
	got, _ := m.Get(high.Key)
	if string(got.Value) != "winner" {
		t.Fatalf("Value = %q, want %q", got.Value, "winner")
	}
}

func TestMapLWWTieBreakerNodeID(t *testing.T) {
	m, _ := testMap(t)
	id1, priv1 := testID(t)
	id2, priv2 := testID(t)

	e1 := testSignedEntry(id1, priv1, "key", []byte("from-1"), 5, false)
	e2 := testSignedEntry(id2, priv2, "key", []byte("from-2"), 5, false)
	m.Merge(e1)
	m.Merge(e2)

	// Both have same ts=5 — higher nodeID wins
	winner := id1
	wantVal := "from-1"
	if id2 > id1 {
		winner = id2
		wantVal = "from-2"
	}
	got, found := m.Get(winner + "/key")
	if !found {
		t.Fatal("winner key not found")
	}
	if string(got.Value) != wantVal {
		t.Fatalf("Value = %q, want %q", got.Value, wantVal)
	}
}

func TestMapSnapshot(t *testing.T) {
	m, nodeID := testMap(t)
	m.Set("a", []byte("1"))
	m.Set("b", []byte("2"))
	m.Set("c", []byte("3"))

	snap := m.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}

	keys := make(map[string]bool)
	for _, e := range snap {
		keys[e.Key] = true
	}
	for _, k := range []string{"a", "b", "c"} {
		fullKey := nodeID + "/" + k
		if !keys[fullKey] {
			t.Fatalf("missing key %q in snapshot", fullKey)
		}
	}
}

func TestMapLen(t *testing.T) {
	m, _ := testMap(t)
	if m.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", m.Len())
	}
	m.Set("x", []byte("1"))
	if m.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", m.Len())
	}
}

func TestMapChangeHandlerCalledOnSet(t *testing.T) {
	m, nodeID := testMap(t)
	var called Entry
	m.SetChangeHandler(func(entry Entry) {
		called = entry
	})

	m.Set("color", []byte("red"))
	wantKey := nodeID + "/color"
	if called.Key != wantKey || string(called.Value) != "red" {
		t.Fatalf("ChangeHandler not called correctly: got %+v", called)
	}
}

func TestMapChangeHandlerNotCalledOnMerge(t *testing.T) {
	m, _ := testMap(t)
	callCount := 0
	m.SetChangeHandler(func(entry Entry) {
		callCount++
	})

	remoteID, remotePriv := testID(t)
	e := testSignedEntry(remoteID, remotePriv, "key", []byte("val"), 10, false)
	m.Merge(e)
	if callCount != 0 {
		t.Fatalf("ChangeHandler called %d times on Merge, want 0", callCount)
	}
}

func TestMapConcurrentAccess(t *testing.T) {
	m, nodeID := testMap(t)
	remoteID, remotePriv := testID(t)
	var wg sync.WaitGroup
	n := 100

	wg.Add(n * 3)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			m.Set("key", []byte("val"))
		}()
		go func(ts uint64) {
			defer wg.Done()
			e := testSignedEntry(remoteID, remotePriv, "key", []byte("remote"), ts, false)
			m.Merge(e)
		}(uint64(i))
		go func() {
			defer wg.Done()
			m.Get(nodeID + "/key")
		}()
	}
	wg.Wait()

	// Should have at most 2 keys (one per namespace)
	if m.Len() > 2 {
		t.Fatalf("Len() = %d, want <= 2", m.Len())
	}
}

func TestEntryWins(t *testing.T) {
	tests := []struct {
		name string
		a, b Entry
		want bool
	}{
		{"higher ts wins", Entry{LamportTs: 10, NodeID: "a"}, Entry{LamportTs: 5, NodeID: "z"}, true},
		{"lower ts loses", Entry{LamportTs: 5, NodeID: "z"}, Entry{LamportTs: 10, NodeID: "a"}, false},
		{"tie: higher node wins", Entry{LamportTs: 5, NodeID: "z"}, Entry{LamportTs: 5, NodeID: "a"}, true},
		{"tie: lower node loses", Entry{LamportTs: 5, NodeID: "a"}, Entry{LamportTs: 5, NodeID: "z"}, false},
		{"tie: same node", Entry{LamportTs: 5, NodeID: "a"}, Entry{LamportTs: 5, NodeID: "a"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Wins(tt.b); got != tt.want {
				t.Errorf("(%v).Wins(%v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// --- Security validation tests ---

func TestMapRejectsOversizedKey(t *testing.T) {
	m, _ := testMap(t)
	// nodeID is 64 hex chars + "/" = 65 chars prefix. MaxKeyLen=256, so subkey > 191 overflows.
	longSubkey := strings.Repeat("x", MaxKeyLen)
	_, _, err := m.Set(longSubkey, []byte("val"))
	if err != ErrKeyTooLong {
		t.Fatalf("expected ErrKeyTooLong, got %v", err)
	}
}

func TestMapRejectsOversizedValue(t *testing.T) {
	m, _ := testMap(t)
	longVal := make([]byte, MaxValueLen+1)
	_, _, err := m.Set("key", longVal)
	if err != ErrValueTooLong {
		t.Fatalf("expected ErrValueTooLong, got %v", err)
	}
}

func TestMapMergeRejectsOversizedValue(t *testing.T) {
	m, _ := testMap(t)
	remoteID, remotePriv := testID(t)

	e := testSignedEntry(remoteID, remotePriv, "k", make([]byte, MaxValueLen+1), 1, false)
	won := m.Merge(e)
	if won {
		t.Fatal("oversized value should be rejected")
	}
}

func TestMapMergeRejectsUnsafeTimestamp(t *testing.T) {
	m, _ := testMap(t)
	remoteID, remotePriv := testID(t)

	e := testSignedEntry(remoteID, remotePriv, "key", []byte("val"), MaxSafeLamportTs+1, false)
	won := m.Merge(e)
	if won {
		t.Fatal("unsafe timestamp should be rejected")
	}

	// Clock should NOT have been advanced
	if m.Clock().Current() != 0 {
		t.Fatalf("clock should still be 0, got %d", m.Clock().Current())
	}
}

func TestMapRejectsNewKeysAtCapacity(t *testing.T) {
	m, _ := testMap(t)
	remoteID, remotePriv := testID(t)

	// Fill to capacity
	for i := 0; i < MaxStateEntries; i++ {
		e := testSignedEntry(remoteID, remotePriv, fmt.Sprintf("k%d", i), []byte("v"), uint64(i+1), false)
		m.Merge(e)
	}
	if m.Len() != MaxStateEntries {
		t.Fatalf("Len() = %d, want %d", m.Len(), MaxStateEntries)
	}

	// New key from a different node should be rejected
	remote2ID, remote2Priv := testID(t)
	e := testSignedEntry(remote2ID, remote2Priv, "new", []byte("v"), 999999, false)
	won := m.Merge(e)
	if won {
		t.Fatal("new key at capacity should be rejected")
	}

	// Update to existing key should still work
	update := testSignedEntry(remoteID, remotePriv, "k0", []byte("updated"), 999999, false)
	won = m.Merge(update)
	if !won {
		t.Fatal("update to existing key should still work at capacity")
	}
}

// --- Signature verification tests ---

func TestMapMergeRejectsUnsignedEntry(t *testing.T) {
	m, _ := testMap(t)
	remoteID, _ := testID(t)

	e := Entry{
		Key:       remoteID + "/key",
		Value:     []byte("val"),
		LamportTs: 1,
		NodeID:    remoteID,
	}
	won := m.Merge(e)
	if won {
		t.Fatal("unsigned entry should be rejected")
	}
}

func TestMapMergeRejectsWrongNamespace(t *testing.T) {
	m, _ := testMap(t)
	remoteID, remotePriv := testID(t)
	otherID, _ := testID(t)

	// Sign entry but put it under another node's namespace
	e := Entry{
		Key:       otherID + "/key",
		Value:     []byte("val"),
		LamportTs: 1,
		NodeID:    remoteID,
	}
	SignEntry(&e, remotePriv)
	won := m.Merge(e)
	if won {
		t.Fatal("entry in wrong namespace should be rejected")
	}
}

func TestMapMergeRejectsTamperedValue(t *testing.T) {
	m, _ := testMap(t)
	remoteID, remotePriv := testID(t)

	e := testSignedEntry(remoteID, remotePriv, "key", []byte("original"), 1, false)
	e.Value = []byte("tampered")
	won := m.Merge(e)
	if won {
		t.Fatal("tampered entry should be rejected")
	}
}

// --- Tombstone tests ---

func TestMapDeleteAndGet(t *testing.T) {
	m, nodeID := testMap(t)
	m.Set("color", []byte("blue"))

	entry, ok, err := m.Delete("color")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Delete rejected")
	}
	if !entry.Deleted {
		t.Fatal("entry should be marked deleted")
	}
	if entry.LamportTs < 2 {
		t.Fatalf("Delete ts should be > Set ts, got %d", entry.LamportTs)
	}

	_, found := m.Get(nodeID + "/color")
	if found {
		t.Fatal("Get should return false for deleted key")
	}
}

func TestMapDeleteNonExistentKey(t *testing.T) {
	m, _ := testMap(t)
	entry, ok, err := m.Delete("missing")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Delete of non-existent key should succeed (creates tombstone)")
	}
	if !entry.Deleted {
		t.Fatal("entry should be marked deleted")
	}
	if m.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", m.Len())
	}
}

func TestMapDeleteWinsOverOlderSet(t *testing.T) {
	m, _ := testMap(t)
	remoteID, remotePriv := testID(t)

	e := testSignedEntry(remoteID, remotePriv, "key", []byte("val"), 1, false)
	m.Merge(e)

	tomb := testSignedEntry(remoteID, remotePriv, "key", nil, 100, true)
	won := m.Merge(tomb)
	if !won {
		t.Fatal("tombstone with higher ts should win")
	}
	_, found := m.Get(remoteID + "/key")
	if found {
		t.Fatal("key should be hidden after tombstone")
	}
}

func TestMapSetResurrectsDelete(t *testing.T) {
	m, nodeID := testMap(t)
	m.Set("key", []byte("first"))
	m.Delete("key")

	entry, ok, _ := m.Set("key", []byte("resurrected"))
	if !ok {
		t.Fatal("Set after Delete should win")
	}
	if entry.Deleted {
		t.Fatal("new Set should not be deleted")
	}

	got, found := m.Get(nodeID + "/key")
	if !found {
		t.Fatal("key should be visible after resurrection")
	}
	if string(got.Value) != "resurrected" {
		t.Fatalf("Value = %q, want %q", got.Value, "resurrected")
	}
}

func TestMapDeleteValidation(t *testing.T) {
	m, _ := testMap(t)
	longSubkey := strings.Repeat("x", MaxKeyLen)
	_, _, err := m.Delete(longSubkey)
	if err != ErrKeyTooLong {
		t.Fatalf("expected ErrKeyTooLong, got %v", err)
	}
}

func TestMapDeleteNotifiesChangeHandler(t *testing.T) {
	m, nodeID := testMap(t)
	var called Entry
	m.SetChangeHandler(func(entry Entry) {
		called = entry
	})

	m.Delete("key")
	wantKey := nodeID + "/key"
	if called.Key != wantKey || !called.Deleted {
		t.Fatalf("ChangeHandler not called on Delete: got %+v", called)
	}
}

func TestMapSnapshotIncludesTombstones(t *testing.T) {
	m, nodeID := testMap(t)
	m.Set("live", []byte("val"))
	m.Set("doomed", []byte("val"))
	m.Delete("doomed")

	snap := m.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2 (including tombstone)", len(snap))
	}

	doomedKey := nodeID + "/doomed"
	var foundTombstone bool
	for _, e := range snap {
		if e.Key == doomedKey && e.Deleted {
			foundTombstone = true
		}
	}
	if !foundTombstone {
		t.Fatal("Snapshot should include tombstone for 'doomed'")
	}
}

func TestMapGC(t *testing.T) {
	m, nodeID := testMap(t)
	m.Set("live", []byte("val"))
	m.Delete("dead")

	deadKey := nodeID + "/dead"
	removed := m.GC(0)
	if len(removed) != 1 || removed[0] != deadKey {
		t.Fatalf("GC removed = %v, want [%s]", removed, deadKey)
	}
	if m.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 after GC", m.Len())
	}
	_, found := m.Get(nodeID + "/live")
	if !found {
		t.Fatal("live entry should survive GC")
	}
}

func TestMapGCPreservesRecentTombstones(t *testing.T) {
	m, _ := testMap(t)
	m.Delete("recent")

	removed := m.GC(24 * time.Hour)
	if len(removed) != 0 {
		t.Fatalf("GC removed %d entries, want 0", len(removed))
	}
	if m.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", m.Len())
	}
}

func TestMapGCResurrectedKeyNotCollected(t *testing.T) {
	m, nodeID := testMap(t)
	m.Delete("key")
	m.Set("key", []byte("alive"))

	removed := m.GC(0)
	if len(removed) != 0 {
		t.Fatalf("GC removed %d entries, want 0 (key was resurrected)", len(removed))
	}

	got, found := m.Get(nodeID + "/key")
	if !found || string(got.Value) != "alive" {
		t.Fatal("resurrected key should survive GC")
	}
}

// --- Digest / SnapshotDelta tests ---

func TestDigest(t *testing.T) {
	m, nodeID := testMap(t)
	m.Set("a", []byte("1")) // ts=1
	m.Set("b", []byte("2")) // ts=2

	remoteID, remotePriv := testID(t)
	e := testSignedEntry(remoteID, remotePriv, "x", []byte("v"), 50, false)
	m.Merge(e)

	d := m.Digest()
	if len(d) != 2 {
		t.Fatalf("Digest len = %d, want 2", len(d))
	}
	if d[nodeID] != 2 {
		t.Fatalf("Digest[local] = %d, want 2", d[nodeID])
	}
	if d[remoteID] != 50 {
		t.Fatalf("Digest[remote] = %d, want 50", d[remoteID])
	}
}

func TestDigestEmpty(t *testing.T) {
	m, _ := testMap(t)
	d := m.Digest()
	if len(d) != 0 {
		t.Fatalf("Digest len = %d, want 0", len(d))
	}
}

func TestSnapshotDelta(t *testing.T) {
	m, nodeID := testMap(t)
	m.Set("a", []byte("1")) // ts=1
	m.Set("b", []byte("2")) // ts=2

	remoteID, remotePriv := testID(t)
	e1 := testSignedEntry(remoteID, remotePriv, "x", []byte("old"), 10, false)
	e2 := testSignedEntry(remoteID, remotePriv, "y", []byte("new"), 50, false)
	m.Merge(e1)
	m.Merge(e2)

	// Peer already knows: local up to ts=1, remote up to ts=10
	known := map[string]uint64{
		nodeID:   1,
		remoteID: 10,
	}
	delta := m.SnapshotDelta(known)

	// Should get: local "b" (ts=2 > 1) and remote "y" (ts=50 > 10)
	if len(delta) != 2 {
		t.Fatalf("SnapshotDelta len = %d, want 2", len(delta))
	}
	keys := make(map[string]bool)
	for _, e := range delta {
		keys[e.Key] = true
	}
	if !keys[nodeID+"/b"] {
		t.Fatal("delta should include local 'b' (ts=2 > known=1)")
	}
	if !keys[remoteID+"/y"] {
		t.Fatal("delta should include remote 'y' (ts=50 > known=10)")
	}
}

func TestSnapshotDeltaEmptyKnown(t *testing.T) {
	m, _ := testMap(t)
	m.Set("a", []byte("1"))
	m.Set("b", []byte("2"))

	remoteID, remotePriv := testID(t)
	e := testSignedEntry(remoteID, remotePriv, "x", []byte("v"), 10, false)
	m.Merge(e)

	// Empty known = send everything (fresh node)
	delta := m.SnapshotDelta(nil)
	if len(delta) != 3 {
		t.Fatalf("SnapshotDelta(nil) len = %d, want 3", len(delta))
	}

	delta = m.SnapshotDelta(map[string]uint64{})
	if len(delta) != 3 {
		t.Fatalf("SnapshotDelta({}) len = %d, want 3", len(delta))
	}
}

func TestSnapshotDeltaUnknownNode(t *testing.T) {
	m, nodeID := testMap(t)
	m.Set("a", []byte("1"))

	remoteID, remotePriv := testID(t)
	e := testSignedEntry(remoteID, remotePriv, "x", []byte("v"), 10, false)
	m.Merge(e)

	// Peer knows local node but not remote — should get all remote entries
	known := map[string]uint64{
		nodeID: 999, // knows all local entries
	}
	delta := m.SnapshotDelta(known)
	if len(delta) != 1 {
		t.Fatalf("SnapshotDelta len = %d, want 1 (only unknown remote)", len(delta))
	}
	if delta[0].NodeID != remoteID {
		t.Fatalf("delta entry NodeID = %q, want %q", delta[0].NodeID, remoteID)
	}
}

// --- Benchmarks: 1000 nodes, 100 updates each ---

// benchNode holds a pre-generated identity for benchmarks.
type benchNode struct {
	id   string
	priv ed25519.PrivateKey
}

func benchGenID() (string, ed25519.PrivateKey) {
	_, priv, _ := ed25519.GenerateKey(nil)
	pub := priv.Public().(ed25519.PublicKey)
	hash := sha256.Sum256(pub)
	return hex.EncodeToString(hash[:]), priv
}

// populateBenchMap creates a map with numNodes nodes. Each node writes
// updatesPerNode times to its "status" key (LWW keeps only the latest).
// Node i's final timestamp is (i % updatesPerNode) + 1, giving a realistic
// spread of update times across the network.
func populateBenchMap(numNodes, updatesPerNode int) (*Map, []benchNode) {
	localID, localPriv := benchGenID()
	m := NewMap(localID, localPriv)

	nodes := make([]benchNode, numNodes)
	for i := range nodes {
		nodes[i].id, nodes[i].priv = benchGenID()
	}

	// Simulate each node updating at staggered times. Node i's latest
	// update has ts = (i % updatesPerNode) + 1, so timestamps are
	// uniformly distributed from 1 to updatesPerNode.
	for i, n := range nodes {
		finalTs := uint64(i%updatesPerNode) + 1
		e := Entry{
			Key:       n.id + "/status",
			Value:     []byte(fmt.Sprintf("round-%d", finalTs)),
			LamportTs: finalTs,
			NodeID:    n.id,
		}
		SignEntry(&e, n.priv)
		m.mu.Lock()
		m.entries[e.Key] = e
		m.mu.Unlock()
	}

	return m, nodes
}

// buildDigestAt builds a version vector where the peer last synced at ts=syncedAt.
// Nodes with final ts <= syncedAt are already known.
func buildDigestAt(nodes []benchNode, updatesPerNode, syncedAt int) map[string]uint64 {
	d := make(map[string]uint64, len(nodes))
	for i, n := range nodes {
		finalTs := uint64(i%updatesPerNode) + 1
		if finalTs <= uint64(syncedAt) {
			d[n.id] = finalTs // peer knows this node's latest
		}
		// Nodes with ts > syncedAt are NOT in the digest (peer doesn't have them)
	}
	return d
}

func BenchmarkDigest1000Nodes(b *testing.B) {
	m, _ := populateBenchMap(1000, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Digest()
	}
}

func BenchmarkSnapshot1000Nodes(b *testing.B) {
	m, _ := populateBenchMap(1000, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Snapshot()
	}
}

func BenchmarkSnapshotDeltaFreshPeer(b *testing.B) {
	m, _ := populateBenchMap(1000, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.SnapshotDelta(nil)
	}
}

func BenchmarkSnapshotDelta1PercentStale(b *testing.B) {
	// Peer synced at ts=99 — misses only the ~1% of nodes with ts=100.
	m, nodes := populateBenchMap(1000, 100)
	stale := buildDigestAt(nodes, 100, 99)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.SnapshotDelta(stale)
	}
}

func BenchmarkSnapshotDelta50PercentStale(b *testing.B) {
	m, nodes := populateBenchMap(1000, 100)
	stale := buildDigestAt(nodes, 100, 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.SnapshotDelta(stale)
	}
}

func BenchmarkSnapshotDeltaFullyCaughtUp(b *testing.B) {
	m, nodes := populateBenchMap(1000, 100)
	caughtUp := buildDigestAt(nodes, 100, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.SnapshotDelta(caughtUp)
	}
}

// TestDeltaSyncSavings demonstrates delta sync savings with 1000 nodes.
// Node i's latest ts = (i % 100) + 1, giving a uniform spread 1..100.
func TestDeltaSyncSavings(t *testing.T) {
	const numNodes = 1000
	const updatesPerNode = 100

	m, nodes := populateBenchMap(numNodes, updatesPerNode)
	full := m.Snapshot()
	digest := m.Digest()

	scenarios := []struct {
		name     string
		syncedAt int
	}{
		{"fresh peer (knows nothing)", 0},
		{"50% stale (synced at ts=50)", 50},
		{"10% stale (synced at ts=90)", 90},
		{"1% stale (synced at ts=99)", 99},
		{"fully caught up (ts=100)", 100},
	}

	t.Logf("")
	t.Logf("=== Delta Sync: %d nodes, ts spread 1..%d ===", numNodes, updatesPerNode)
	t.Logf("  Total entries in map:  %d", len(full))
	t.Logf("  Digest size:           %d nodeIDs (~%d bytes)", len(digest), len(digest)*72)
	t.Logf("")

	for _, sc := range scenarios {
		var known map[string]uint64
		if sc.syncedAt > 0 {
			known = buildDigestAt(nodes, updatesPerNode, sc.syncedAt)
		}
		delta := m.SnapshotDelta(known)
		saving := 100 * (1 - float64(len(delta))/float64(len(full)))
		t.Logf("  %-35s → %4d entries (saving %5.1f%%)", sc.name, len(delta), saving)
	}

	// Correctness checks
	if len(full) != numNodes {
		t.Fatalf("expected %d entries, got %d", numNodes, len(full))
	}
	freshDelta := m.SnapshotDelta(nil)
	if len(freshDelta) != numNodes {
		t.Fatalf("fresh peer should get all %d entries, got %d", numNodes, len(freshDelta))
	}
	caughtUp := buildDigestAt(nodes, updatesPerNode, updatesPerNode)
	upDelta := m.SnapshotDelta(caughtUp)
	if len(upDelta) != 0 {
		t.Fatalf("caught-up peer should get 0 entries, got %d", len(upDelta))
	}
}

package transport_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/logging"
	"micelio/internal/partyline"
	"micelio/internal/state"
	boltstore "micelio/internal/store/bolt"
	"micelio/internal/transport"
)

// --- Integration test helpers ---

// stateTestNode wraps all components needed for a mesh node in state tests.
type stateTestNode struct {
	id     *identity.Identity
	hub    *partyline.Hub
	mgr    *transport.Manager
	ctx    context.Context //nolint:containedctx // acceptable in test helpers
	cancel context.CancelFunc
}

func newStateTestNode(t *testing.T, name string) *stateTestNode {
	t.Helper()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatalf("identity %s: %v", name, err)
	}
	hub := partyline.NewHub(name)
	ctx, cancel := context.WithCancel(context.Background())
	return &stateTestNode{id: id, hub: hub, ctx: ctx, cancel: cancel}
}

func (n *stateTestNode) start(t *testing.T, name string, bootstrap []string) {
	t.Helper()
	cfg := &config.Config{
		Node: config.NodeConfig{Name: name},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			Bootstrap:         bootstrap,
			ExchangeInterval:  noDiscovery,
			DiscoveryInterval: noDiscovery,
		},
	}
	mgr, err := transport.NewManager(cfg, n.id, n.hub, nil)
	if err != nil {
		t.Fatalf("manager %s: %v", name, err)
	}
	n.mgr = mgr
	go n.hub.Run()
	go func() { _ = mgr.Start(n.ctx) }()
}

func (n *stateTestNode) cleanup() {
	if n.mgr != nil {
		n.mgr.Stop()
	}
	n.cancel()
	n.hub.Stop()
}

func waitAddr(t *testing.T, mgr *transport.Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for mgr.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if mgr.Addr() == "" {
		t.Fatal("manager did not bind")
	}
}

func waitPeers(t *testing.T, managers ...*transport.Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok := true
		for _, m := range managers {
			if m.PeerCount() == 0 {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("nodes not connected")
}

// startPair creates two nodes, starts them, and waits until they are connected.
// nodeB is the listener, nodeA bootstraps to B.
func startPair(t *testing.T) (a, b *stateTestNode) {
	t.Helper()
	b = newStateTestNode(t, "B")
	b.start(t, "node-b", nil)
	waitAddr(t, b.mgr)

	a = newStateTestNode(t, "A")
	a.start(t, "node-a", []string{b.mgr.Addr()})
	waitPeers(t, a.mgr, b.mgr)
	return a, b
}

func pollGet(t *testing.T, sm *state.Map, key, wantVal string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok := sm.Get(key); ok && string(e.Value) == wantVal {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("key %q did not converge to %q", key, wantVal)
}

func pollGone(t *testing.T, sm *state.Map, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := sm.Get(key); !ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("key %q still present", key)
}

// --- Tests ---

// TestStateSyncTwoNodes verifies that a /set on node A is visible on node B
// after gossip propagation. Keys are namespaced by nodeID.
func TestStateSyncTwoNodes(t *testing.T) {
	logging.CaptureForTest()
	a, b := startPair(t)
	defer func() { b.cleanup(); a.cleanup() }()

	// Set on A (subkey "color" → full key "<nodeA_ID>/color")
	stateA := a.mgr.StateMap()
	entry, ok, err := stateA.Set("color", []byte("blue"))
	if err != nil {
		t.Fatalf("Set error: %v", err)
	}
	if !ok {
		t.Fatal("Set rejected on A")
	}
	if entry.LamportTs == 0 {
		t.Fatal("LamportTs should be > 0")
	}

	// Poll B until it has the value (using full key from A's namespace)
	fullKey := a.id.NodeID + "/color"
	stateB := b.mgr.StateMap()
	pollGet(t, stateB, fullKey, "blue")

	// Verify entry details
	got, _ := stateB.Get(fullKey)
	if got.LamportTs != entry.LamportTs {
		t.Fatalf("B LamportTs = %d, want %d", got.LamportTs, entry.LamportTs)
	}
	if got.NodeID != a.id.NodeID {
		t.Fatalf("B NodeID = %q, want %q", got.NodeID, a.id.NodeID)
	}
	if !state.VerifyEntry(got) {
		t.Fatal("entry signature invalid after gossip propagation")
	}
}

// TestStateConflictDeterministicWinner verifies that LWW convergence works
// with namespaced, signed entries. Each node writes under its own namespace.
func TestStateConflictDeterministicWinner(t *testing.T) {
	_, privA, _ := ed25519.GenerateKey(nil)
	pubA := privA.Public().(ed25519.PublicKey)
	hashA := sha256.Sum256(pubA)
	nodeIDA := hex.EncodeToString(hashA[:])

	_, privB, _ := ed25519.GenerateKey(nil)
	pubB := privB.Public().(ed25519.PublicKey)
	hashB := sha256.Sum256(pubB)
	nodeIDB := hex.EncodeToString(hashB[:])

	mapA := state.NewMap(nodeIDA, privA)
	mapB := state.NewMap(nodeIDB, privB)

	// Each node writes to its own namespace
	entryA, _, _ := mapA.Set("key", []byte("from-A"))
	entryB, _, _ := mapB.Set("key", []byte("from-B"))

	if entryA.LamportTs != 1 || entryB.LamportTs != 1 {
		t.Fatalf("expected both ts=1, got A=%d B=%d", entryA.LamportTs, entryB.LamportTs)
	}

	// Cross-merge: both nodes receive each other's entries
	mapA.Merge(entryB)
	mapB.Merge(entryA)

	// Both should see both entries (different namespaces, no conflict)
	gotA1, okA1 := mapA.Get(nodeIDA + "/key")
	gotA2, okA2 := mapA.Get(nodeIDB + "/key")
	if !okA1 || !okA2 {
		t.Fatalf("mapA should have both entries: own=%v remote=%v", okA1, okA2)
	}
	if string(gotA1.Value) != "from-A" || string(gotA2.Value) != "from-B" {
		t.Fatalf("mapA values: own=%q remote=%q", gotA1.Value, gotA2.Value)
	}

	gotB1, okB1 := mapB.Get(nodeIDA + "/key")
	gotB2, okB2 := mapB.Get(nodeIDB + "/key")
	if !okB1 || !okB2 {
		t.Fatalf("mapB should have both entries: remote=%v own=%v", okB1, okB2)
	}
	if string(gotB1.Value) != "from-A" || string(gotB2.Value) != "from-B" {
		t.Fatalf("mapB values: remote=%q own=%q", gotB1.Value, gotB2.Value)
	}
}

// TestStateSyncOnConnect verifies that when node B connects to node A,
// B receives A's pre-existing state via the state sync protocol.
func TestStateSyncOnConnect(t *testing.T) {
	logging.CaptureForTest()

	// Start A alone and set state BEFORE B connects
	a := newStateTestNode(t, "A")
	a.start(t, "node-a", nil)
	defer a.cleanup()
	waitAddr(t, a.mgr)

	// Set state on A before B connects
	a.mgr.StateMap().Set("x", []byte("1")) //nolint:errcheck
	a.mgr.StateMap().Set("y", []byte("2")) //nolint:errcheck

	// Now start B and connect to A
	b := newStateTestNode(t, "B")
	b.start(t, "node-b", []string{a.mgr.Addr()})
	defer b.cleanup()
	waitPeers(t, b.mgr)

	// Poll B for both keys (using A's namespace prefix)
	stateB := b.mgr.StateMap()
	pollGet(t, stateB, a.id.NodeID+"/x", "1")
	pollGet(t, stateB, a.id.NodeID+"/y", "2")
}

// TestStatePersistenceRestart verifies that state survives a node restart
// by persisting to bbolt and loading on startup.
func TestStatePersistenceRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Generate a stable identity for both runs
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	hash := sha256.Sum256(pub)
	nodeID := hex.EncodeToString(hash[:])

	// First "run": create store, state map, set entries
	st1, err := boltstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	m1 := state.NewMap(nodeID, priv)
	if err := m1.LoadFromStore(st1); err != nil {
		t.Fatal(err)
	}

	e1, _, _ := m1.Set("color", []byte("red"))
	state.PersistEntry(st1, e1)
	e2, _, _ := m1.Set("size", []byte("large"))
	state.PersistEntry(st1, e2)

	_ = st1.Close()

	// Second "run": reopen store, create new map with same identity, load
	st2, err := boltstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()

	m2 := state.NewMap(nodeID, priv)
	if err := m2.LoadFromStore(st2); err != nil {
		t.Fatal(err)
	}

	// Verify entries are restored
	if m2.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", m2.Len())
	}

	got, ok := m2.Get(nodeID + "/color")
	if !ok || string(got.Value) != "red" {
		t.Fatalf("color = %q, want %q", got.Value, "red")
	}

	got, ok = m2.Get(nodeID + "/size")
	if !ok || string(got.Value) != "large" {
		t.Fatalf("size = %q, want %q", got.Value, "large")
	}

	// Verify clock floor: next tick should be > max persisted ts
	nextTs := m2.Clock().Tick()
	if nextTs <= e2.LamportTs {
		t.Fatalf("clock tick %d should be > max persisted ts %d", nextTs, e2.LamportTs)
	}
}

// TestDeletePropagatesTwoNodes verifies that a /del on node A causes
// Get on node B to return not-found after gossip propagation.
func TestDeletePropagatesTwoNodes(t *testing.T) {
	logging.CaptureForTest()
	a, b := startPair(t)
	defer func() { b.cleanup(); a.cleanup() }()

	// Set on A, wait for B to receive it
	stateA := a.mgr.StateMap()
	stateA.Set("color", []byte("blue")) //nolint:errcheck

	colorKey := a.id.NodeID + "/color"
	pollGet(t, b.mgr.StateMap(), colorKey, "blue")

	// Delete on A, wait for B to see it gone
	stateA.Delete("color") //nolint:errcheck
	pollGone(t, b.mgr.StateMap(), colorKey)
}

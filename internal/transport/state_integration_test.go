package transport_test

import (
	"context"
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

// TestStateSyncTwoNodes verifies that a /set on node A is visible on node B
// after gossip propagation.
func TestStateSyncTwoNodes(t *testing.T) {
	logging.CaptureForTest()

	type testNode struct {
		id     *identity.Identity
		hub    *partyline.Hub
		mgr    *transport.Manager
		ctx    context.Context
		cancel context.CancelFunc
	}

	makeNode := func(name string) testNode {
		id, err := identity.Load(t.TempDir())
		if err != nil {
			t.Fatalf("identity %s: %v", name, err)
		}
		hub := partyline.NewHub(name)
		ctx, cancel := context.WithCancel(context.Background())
		return testNode{id: id, hub: hub, ctx: ctx, cancel: cancel}
	}

	cleanup := func(n *testNode) {
		if n.mgr != nil {
			n.mgr.Stop()
		}
		n.cancel()
		n.hub.Stop()
	}

	nodeA := makeNode("node-a")
	nodeB := makeNode("node-b")
	defer func() {
		cleanup(&nodeB)
		cleanup(&nodeA)
	}()

	// Start B (listener)
	cfgB := &config.Config{
		Node: config.NodeConfig{Name: "node-b"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			ExchangeInterval:  noDiscovery,
			DiscoveryInterval: noDiscovery,
		},
	}
	mgrB, err := transport.NewManager(cfgB, nodeB.id, nodeB.hub, nil)
	if err != nil {
		t.Fatalf("manager B: %v", err)
	}
	nodeB.mgr = mgrB
	go nodeB.hub.Run()
	go func() { _ = mgrB.Start(nodeB.ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for mgrB.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Start A (bootstraps to B)
	cfgA := &config.Config{
		Node: config.NodeConfig{Name: "node-a"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			Bootstrap:         []string{mgrB.Addr()},
			ExchangeInterval:  noDiscovery,
			DiscoveryInterval: noDiscovery,
		},
	}
	mgrA, err := transport.NewManager(cfgA, nodeA.id, nodeA.hub, nil)
	if err != nil {
		t.Fatalf("manager A: %v", err)
	}
	nodeA.mgr = mgrA
	go nodeA.hub.Run()
	go func() { _ = mgrA.Start(nodeA.ctx) }()

	// Wait for connection
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgrA.PeerCount() > 0 && mgrB.PeerCount() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgrA.PeerCount() == 0 {
		t.Fatal("nodes not connected")
	}

	// Set on A
	stateA := mgrA.StateMap()
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

	// Poll B until it has the value
	stateB := mgrB.StateMap()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e, found := stateB.Get("color"); found && string(e.Value) == "blue" {
			// Verify metadata
			if e.LamportTs != entry.LamportTs {
				t.Fatalf("B LamportTs = %d, want %d", e.LamportTs, entry.LamportTs)
			}
			if e.NodeID != nodeA.id.NodeID {
				t.Fatalf("B NodeID = %q, want %q", e.NodeID, nodeA.id.NodeID)
			}
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("B did not receive state update from A")
}

// TestStateConflictDeterministicWinner verifies that both nodes converge to
// the same winner when they write to the same key with the same timestamp.
func TestStateConflictDeterministicWinner(t *testing.T) {
	// Use maps directly (no network) to test pure LWW resolution.
	mapA := state.NewMap("node-aaaa")
	mapB := state.NewMap("node-bbbb")

	// Both write to the same key. mapA gets ts=1, mapB gets ts=1.
	entryA, _, _ := mapA.Set("key", []byte("from-A"))
	entryB, _, _ := mapB.Set("key", []byte("from-B"))

	if entryA.LamportTs != 1 || entryB.LamportTs != 1 {
		t.Fatalf("expected both ts=1, got A=%d B=%d", entryA.LamportTs, entryB.LamportTs)
	}

	// Cross-merge: A receives B's entry, B receives A's entry
	mapA.Merge(entryB)
	mapB.Merge(entryA)

	// Both should converge to the same winner
	gotA, _ := mapA.Get("key")
	gotB, _ := mapB.Get("key")

	if string(gotA.Value) != string(gotB.Value) {
		t.Fatalf("maps diverged: A=%q B=%q", gotA.Value, gotB.Value)
	}

	// The winner should be the one with the higher node_id (node-bbbb > node-aaaa)
	if string(gotA.Value) != "from-B" {
		t.Fatalf("expected 'from-B' to win (higher node_id), got %q", gotA.Value)
	}
}

// TestStateSyncOnConnect verifies that when node B connects to node A,
// B receives A's pre-existing state via the state sync protocol.
func TestStateSyncOnConnect(t *testing.T) {
	logging.CaptureForTest()

	type testNode struct {
		id     *identity.Identity
		hub    *partyline.Hub
		mgr    *transport.Manager
		ctx    context.Context
		cancel context.CancelFunc
	}

	cleanup := func(n *testNode) {
		if n.mgr != nil {
			n.mgr.Stop()
		}
		n.cancel()
		n.hub.Stop()
	}

	// Start A and set state BEFORE B connects
	idA, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hubA := partyline.NewHub("node-a")
	ctxA, cancelA := context.WithCancel(context.Background())
	nodeA := testNode{id: idA, hub: hubA, ctx: ctxA, cancel: cancelA}
	defer cleanup(&nodeA)

	cfgA := &config.Config{
		Node: config.NodeConfig{Name: "node-a"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			ExchangeInterval:  noDiscovery,
			DiscoveryInterval: noDiscovery,
		},
	}
	mgrA, err := transport.NewManager(cfgA, idA, hubA, nil)
	if err != nil {
		t.Fatal(err)
	}
	nodeA.mgr = mgrA
	go hubA.Run()
	go func() { _ = mgrA.Start(ctxA) }()

	deadline := time.Now().Add(5 * time.Second)
	for mgrA.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Set state on A before B connects
	mgrA.StateMap().Set("x", []byte("1"))   //nolint:errcheck
	mgrA.StateMap().Set("y", []byte("2"))   //nolint:errcheck

	// Now start B and connect to A
	idB, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hubB := partyline.NewHub("node-b")
	ctxB, cancelB := context.WithCancel(context.Background())
	nodeB := testNode{id: idB, hub: hubB, ctx: ctxB, cancel: cancelB}
	defer cleanup(&nodeB)

	cfgB := &config.Config{
		Node: config.NodeConfig{Name: "node-b"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			Bootstrap:         []string{mgrA.Addr()},
			ExchangeInterval:  noDiscovery,
			DiscoveryInterval: noDiscovery,
		},
	}
	mgrB, err := transport.NewManager(cfgB, idB, hubB, nil)
	if err != nil {
		t.Fatal(err)
	}
	nodeB.mgr = mgrB
	go hubB.Run()
	go func() { _ = mgrB.Start(ctxB) }()

	// Wait for connection
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgrB.PeerCount() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgrB.PeerCount() == 0 {
		t.Fatal("B not connected to A")
	}

	// Poll B for both keys (should arrive via state sync)
	stateB := mgrB.StateMap()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ex, okX := stateB.Get("x")
		ey, okY := stateB.Get("y")
		if okX && okY && string(ex.Value) == "1" && string(ey.Value) == "2" {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("B did not receive A's pre-existing state via sync")
}

// TestStatePersistenceRestart verifies that state survives a node restart
// by persisting to bbolt and loading on startup.
func TestStatePersistenceRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// First "run": create store, state map, set entries
	st1, err := boltstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	m1 := state.NewMap("node-a")
	if err := m1.LoadFromStore(st1); err != nil {
		t.Fatal(err)
	}

	e1, _, _ := m1.Set("color", []byte("red"))
	state.PersistEntry(st1, e1)
	e2, _, _ := m1.Set("size", []byte("large"))
	state.PersistEntry(st1, e2)

	_ = st1.Close()

	// Second "run": reopen store, create new map, load
	st2, err := boltstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()

	m2 := state.NewMap("node-a")
	if err := m2.LoadFromStore(st2); err != nil {
		t.Fatal(err)
	}

	// Verify entries are restored
	if m2.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", m2.Len())
	}

	got, ok := m2.Get("color")
	if !ok || string(got.Value) != "red" {
		t.Fatalf("color = %q, want %q", got.Value, "red")
	}

	got, ok = m2.Get("size")
	if !ok || string(got.Value) != "large" {
		t.Fatalf("size = %q, want %q", got.Value, "large")
	}

	// Verify clock floor: next tick should be > max persisted ts
	nextTs := m2.Clock().Tick()
	if nextTs <= e2.LamportTs {
		t.Fatalf("clock tick %d should be > max persisted ts %d", nextTs, e2.LamportTs)
	}
}

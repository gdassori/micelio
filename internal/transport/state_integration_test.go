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

// TestStateSyncTwoNodes verifies that a /set on node A is visible on node B
// after gossip propagation. Keys are namespaced by nodeID.
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

	// Set on A (subkey "color" → full key "<nodeA_ID>/color")
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

	// Poll B until it has the value (using full key from A's namespace)
	fullKey := nodeA.id.NodeID + "/color"
	stateB := mgrB.StateMap()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e, found := stateB.Get(fullKey); found && string(e.Value) == "blue" {
			if e.LamportTs != entry.LamportTs {
				t.Fatalf("B LamportTs = %d, want %d", e.LamportTs, entry.LamportTs)
			}
			if e.NodeID != nodeA.id.NodeID {
				t.Fatalf("B NodeID = %q, want %q", e.NodeID, nodeA.id.NodeID)
			}
			// Verify the entry signature is valid after gossip propagation
			if !state.VerifyEntry(e) {
				t.Fatal("entry signature invalid after gossip propagation")
			}
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("B did not receive state update from A")
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

	// Set state on A before B connects (subkeys auto-prefixed with nodeA ID)
	mgrA.StateMap().Set("x", []byte("1")) //nolint:errcheck
	mgrA.StateMap().Set("y", []byte("2")) //nolint:errcheck

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

	// Poll B for both keys (using A's namespace prefix)
	keyX := idA.NodeID + "/x"
	keyY := idA.NodeID + "/y"
	stateB := mgrB.StateMap()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ex, okX := stateB.Get(keyX)
		ey, okY := stateB.Get(keyY)
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

	colorKey := nodeID + "/color"
	got, ok := m2.Get(colorKey)
	if !ok || string(got.Value) != "red" {
		t.Fatalf("color = %q, want %q", got.Value, "red")
	}

	sizeKey := nodeID + "/size"
	got, ok = m2.Get(sizeKey)
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

	// Set on A, wait for B to receive it
	stateA := mgrA.StateMap()
	stateA.Set("color", []byte("blue")) //nolint:errcheck

	colorKey := nodeA.id.NodeID + "/color"
	stateB := mgrB.StateMap()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := stateB.Get(colorKey); found {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, found := stateB.Get(colorKey); !found {
		t.Fatal("B did not receive Set from A")
	}

	// Delete on A
	stateA.Delete("color") //nolint:errcheck

	// Poll B until it no longer has the value
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := stateB.Get(colorKey); !found {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("B did not receive Delete from A")
}

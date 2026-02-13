package transport_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/logging"
	"micelio/internal/partyline"
	"micelio/internal/transport"
)

// TestGossipChainRelay verifies multi-hop gossip propagation.
// Topology: A─B─C (chain, no discovery). A and C are NOT directly connected.
// A sends a message → it should reach C via gossip through B.
func TestGossipChainRelay(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	type testNode struct {
		id      *identity.Identity
		hub     *partyline.Hub
		mgr     *transport.Manager
		ctx     context.Context
		cancel  context.CancelFunc
		session *partyline.Session
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
		if n.session != nil {
			n.hub.Leave(n.session)
		}
		if n.mgr != nil {
			n.mgr.Stop()
		}
		n.cancel()
		n.hub.Stop()
	}

	nodeA := makeNode("node-a")
	nodeB := makeNode("node-b")
	nodeC := makeNode("node-c")
	defer func() {
		cleanup(&nodeC)
		cleanup(&nodeB)
		cleanup(&nodeA)
	}()

	// Start B (center) — listens, no bootstrap
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
	go mgrB.Start(nodeB.ctx)

	deadline := time.Now().Add(5 * time.Second)
	for mgrB.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	addrB := mgrB.Addr()

	// Start A — bootstraps to B only
	cfgA := &config.Config{
		Node: config.NodeConfig{Name: "node-a"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			Bootstrap:         []string{addrB},
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
	go mgrA.Start(nodeA.ctx)

	// Start C — bootstraps to B only (does NOT know A)
	cfgC := &config.Config{
		Node: config.NodeConfig{Name: "node-c"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			Bootstrap:         []string{addrB},
			ExchangeInterval:  noDiscovery,
			DiscoveryInterval: noDiscovery,
		},
	}
	mgrC, err := transport.NewManager(cfgC, nodeC.id, nodeC.hub, nil)
	if err != nil {
		t.Fatalf("manager C: %v", err)
	}
	nodeC.mgr = mgrC
	go nodeC.hub.Run()
	go mgrC.Start(nodeC.ctx)

	// Wait for A↔B and B↔C connections
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if mgrA.PeerCount() >= 1 && mgrC.PeerCount() >= 1 && mgrB.PeerCount() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgrB.PeerCount() < 2 {
		t.Fatalf("B has %d peers, want 2", mgrB.PeerCount())
	}

	// A and C should NOT be directly connected (no discovery, no bootstrap between them)
	if mgrA.PeerCount() != 1 {
		t.Fatalf("A has %d peers, want 1 (only B)", mgrA.PeerCount())
	}
	if mgrC.PeerCount() != 1 {
		t.Fatalf("C has %d peers, want 1 (only B)", mgrC.PeerCount())
	}

	// Join sessions
	nodeA.session = nodeA.hub.Join("alice")
	nodeB.session = nodeB.hub.Join("bob")
	nodeC.session = nodeC.hub.Join("carol")
	drainFor(nodeA.session.Send, 200*time.Millisecond)
	drainFor(nodeB.session.Send, 200*time.Millisecond)
	drainFor(nodeC.session.Send, 200*time.Millisecond)

	// A sends a message
	nodeA.hub.Broadcast(nodeA.session, "hello from A")

	// B should receive (directly connected to A)
	if msg := waitForMsg(nodeB.session.Send, 5*time.Second, "alice", "hello from A"); msg == "" {
		t.Fatal("B did not receive message from A")
	}

	// C should receive via gossip through B!
	if msg := waitForMsg(nodeC.session.Send, 5*time.Second, "alice", "hello from A"); msg == "" {
		t.Fatal("C did not receive message from A via gossip through B")
	}

	// Verify reverse: C sends, A receives via B
	drainFor(nodeA.session.Send, 200*time.Millisecond)
	drainFor(nodeB.session.Send, 200*time.Millisecond)
	drainFor(nodeC.session.Send, 200*time.Millisecond)

	nodeC.hub.Broadcast(nodeC.session, "reply from C")
	if msg := waitForMsg(nodeA.session.Send, 5*time.Second, "carol", "reply from C"); msg == "" {
		t.Fatal("A did not receive message from C via gossip through B")
	}

	if !capture.Has(slog.LevelInfo, "peer connected") {
		t.Error("expected INFO log: peer connected")
	}
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG log: message received")
	}
	if !capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("expected DEBUG log: message forwarded")
	}
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", capture.Count(slog.LevelError))
	}
}

// TestGossipDedup verifies that the same message is not delivered twice even
// when it arrives from multiple peers.
func TestGossipDedup(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	type testNode struct {
		id      *identity.Identity
		hub     *partyline.Hub
		mgr     *transport.Manager
		ctx     context.Context
		cancel  context.CancelFunc
		session *partyline.Session
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
		if n.session != nil {
			n.hub.Leave(n.session)
		}
		if n.mgr != nil {
			n.mgr.Stop()
		}
		n.cancel()
		n.hub.Stop()
	}

	// Triangle topology: A↔B, A↔C, B↔C (full mesh)
	// A sends a message → B and C both forward to each other, but dedup prevents double delivery.
	nodeA := makeNode("node-a")
	nodeB := makeNode("node-b")
	nodeC := makeNode("node-c")
	defer func() {
		cleanup(&nodeC)
		cleanup(&nodeB)
		cleanup(&nodeA)
	}()

	// Start A
	cfgA := &config.Config{
		Node: config.NodeConfig{Name: "node-a"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
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
	go mgrA.Start(nodeA.ctx)

	deadline := time.Now().Add(5 * time.Second)
	for mgrA.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	addrA := mgrA.Addr()

	// Start B — bootstraps to A
	cfgB := &config.Config{
		Node: config.NodeConfig{Name: "node-b"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			Bootstrap:         []string{addrA},
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
	go mgrB.Start(nodeB.ctx)

	// Wait for A↔B
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgrA.PeerCount() >= 1 && mgrB.PeerCount() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Start C — bootstraps to both A and B (forms triangle)
	cfgC := &config.Config{
		Node: config.NodeConfig{Name: "node-c"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			Bootstrap:         []string{addrA, mgrB.Addr()},
			ExchangeInterval:  noDiscovery,
			DiscoveryInterval: noDiscovery,
		},
	}
	mgrC, err := transport.NewManager(cfgC, nodeC.id, nodeC.hub, nil)
	if err != nil {
		t.Fatalf("manager C: %v", err)
	}
	nodeC.mgr = mgrC
	go nodeC.hub.Run()
	go mgrC.Start(nodeC.ctx)

	// Wait for full mesh
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if mgrA.PeerCount() >= 2 && mgrB.PeerCount() >= 2 && mgrC.PeerCount() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgrC.PeerCount() < 2 {
		t.Fatalf("mesh not formed: A=%d B=%d C=%d", mgrA.PeerCount(), mgrB.PeerCount(), mgrC.PeerCount())
	}

	// Join sessions
	nodeA.session = nodeA.hub.Join("alice")
	nodeB.session = nodeB.hub.Join("bob")
	nodeC.session = nodeC.hub.Join("carol")
	drainFor(nodeA.session.Send, 200*time.Millisecond)
	drainFor(nodeB.session.Send, 200*time.Millisecond)
	drainFor(nodeC.session.Send, 200*time.Millisecond)

	// A sends "unique-msg". B and C receive it directly from A.
	// B may try to forward to C and vice versa, but dedup ensures single delivery.
	nodeA.hub.Broadcast(nodeA.session, "unique-msg")

	// B receives exactly once
	if msg := waitForMsg(nodeB.session.Send, 5*time.Second, "alice", "unique-msg"); msg == "" {
		t.Fatal("B did not receive the message")
	}
	// Check no duplicate delivery (wait a bit and see if another comes)
	if msg := waitForMsg(nodeB.session.Send, 500*time.Millisecond, "alice", "unique-msg"); msg != "" {
		t.Fatal("B received duplicate message")
	}

	// C receives exactly once
	if msg := waitForMsg(nodeC.session.Send, 5*time.Second, "alice", "unique-msg"); msg == "" {
		t.Fatal("C did not receive the message")
	}
	if msg := waitForMsg(nodeC.session.Send, 500*time.Millisecond, "alice", "unique-msg"); msg != "" {
		t.Fatal("C received duplicate message")
	}

	if !capture.Has(slog.LevelInfo, "peer connected") {
		t.Error("expected INFO log: peer connected")
	}
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG log: message received")
	}
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", capture.Count(slog.LevelError))
	}
}

// TestGossipDefaultHopCountChain verifies multi-hop gossip propagation using
// the engine's default hop_count in a 5-node chain topology.
// Topology: A─B─C─D─E (chain). A and E are NOT directly connected.
// The message is expected to traverse all intermediate hops.
// Strict hop_count=0/1 behavior is validated in the gossip engine unit tests
// (e.g., TestEngineHopCountZero). Here we exercise the default hop_count
// end-to-end over a realistic 4-hop chain.
func TestGossipDefaultHopCountChain(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	const N = 5
	type testNode struct {
		id      *identity.Identity
		hub     *partyline.Hub
		mgr     *transport.Manager
		ctx     context.Context
		cancel  context.CancelFunc
		session *partyline.Session
	}

	nodes := make([]testNode, N)
	for i := 0; i < N; i++ {
		id, err := identity.Load(t.TempDir())
		if err != nil {
			t.Fatalf("identity node-%d: %v", i, err)
		}
		hub := partyline.NewHub(fmt.Sprintf("node-%d", i))
		ctx, cancel := context.WithCancel(context.Background())
		nodes[i] = testNode{id: id, hub: hub, ctx: ctx, cancel: cancel}
	}

	defer func() {
		for i := N - 1; i >= 0; i-- {
			if nodes[i].session != nil {
				nodes[i].hub.Leave(nodes[i].session)
			}
			if nodes[i].mgr != nil {
				nodes[i].mgr.Stop()
			}
			nodes[i].cancel()
			nodes[i].hub.Stop()
		}
	}()

	// Build chain: 0─1─2─3─4, no discovery (pure chain)
	addrs := make([]string, N)
	for i := N - 1; i >= 0; i-- {
		var bootstrap []string
		if i < N-1 {
			bootstrap = []string{addrs[i+1]}
		}
		cfg := &config.Config{
			Node: config.NodeConfig{Name: fmt.Sprintf("node-%d", i)},
			Network: config.NetworkConfig{
				Listen:            "127.0.0.1:0",
				Bootstrap:         bootstrap,
				ExchangeInterval:  noDiscovery,
				DiscoveryInterval: noDiscovery,
			},
		}
		mgr, err := transport.NewManager(cfg, nodes[i].id, nodes[i].hub, nil)
		if err != nil {
			t.Fatalf("manager %d: %v", i, err)
		}
		nodes[i].mgr = mgr
		go nodes[i].hub.Run()
		go mgr.Start(nodes[i].ctx)

		deadline := time.Now().Add(5 * time.Second)
		for mgr.Addr() == "" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		addrs[i] = mgr.Addr()
	}

	// Wait for chain links
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		allLinked := true
		for i := 0; i < N; i++ {
			if nodes[i].mgr.PeerCount() == 0 {
				allLinked = false
				break
			}
		}
		if allLinked {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Each node has the expected number of peers for the chain topology (no discovery)
	for i := 0; i < N; i++ {
		expected := 1
		if i > 0 && i < N-1 {
			expected = 2 // middle nodes have two neighbors
		}
		got := nodes[i].mgr.PeerCount()
		if got != expected {
			t.Fatalf("node %d has %d peers; expected %d (chain not formed as expected)", i, got, expected)
		}
	}

	// Join sessions
	nicks := []string{"alice", "bob", "carol", "dave", "eve"}
	for i := 0; i < N; i++ {
		nodes[i].session = nodes[i].hub.Join(nicks[i])
	}
	for i := 0; i < N; i++ {
		drainFor(nodes[i].session.Send, 200*time.Millisecond)
	}

	// Node 0 sends — should reach node 4 via gossip through the chain (4 hops)
	nodes[0].hub.Broadcast(nodes[0].session, "chain-gossip-test")

	msg := waitForMsg(nodes[N-1].session.Send, 10*time.Second, nicks[0], "chain-gossip-test")
	if msg == "" {
		t.Fatal("node 4 did not receive message from node 0 via gossip chain")
	}

	// All intermediate nodes should also have received it
	for i := 1; i < N-1; i++ {
		msg := waitForMsg(nodes[i].session.Send, 5*time.Second, nicks[0], "chain-gossip-test")
		if msg == "" {
			t.Fatalf("node %d did not receive message from node 0", i)
		}
	}

	if !capture.Has(slog.LevelInfo, "peer connected") {
		t.Error("expected INFO log: peer connected")
	}
	if !capture.Has(slog.LevelDebug, "message received") {
		t.Error("expected DEBUG log: message received")
	}
	if !capture.Has(slog.LevelDebug, "message forwarded") {
		t.Error("expected DEBUG log: message forwarded")
	}
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", capture.Count(slog.LevelError))
	}
}

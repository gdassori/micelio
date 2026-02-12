package transport_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/partyline"
	"micelio/internal/transport"
)

// TestThreeNodeMesh verifies automatic mesh formation via PeerExchange.
// Topology: A knows B (bootstrap), C knows B (bootstrap), but A does NOT know C.
// After PeerExchange, A should discover C and dial it, forming a full mesh.
func TestThreeNodeMesh(t *testing.T) {
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

	nodeB := makeNode("node-b")
	nodeA := makeNode("node-a")
	nodeC := makeNode("node-c")

	defer func() {
		cleanup(&nodeC)
		cleanup(&nodeA)
		cleanup(&nodeB)
	}()

	// Fast intervals for testing
	fastExchange := config.Duration{Duration: 1 * time.Second}
	fastDiscovery := config.Duration{Duration: 1 * time.Second}

	// Start B (center) — listens, no bootstrap
	cfgB := &config.Config{
		Node: config.NodeConfig{Name: "node-b"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			MaxPeers:          15,
			ExchangeInterval:  fastExchange,
			DiscoveryInterval: fastDiscovery,
		},
	}
	mgrB, err := transport.NewManager(cfgB, nodeB.id, nodeB.hub, nil)
	if err != nil {
		t.Fatalf("manager B: %v", err)
	}
	nodeB.mgr = mgrB
	go nodeB.hub.Run()
	go mgrB.Start(nodeB.ctx)

	// Wait for B's listener
	deadline := time.Now().Add(5 * time.Second)
	for mgrB.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if mgrB.Addr() == "" {
		t.Fatal("node B failed to start listener")
	}
	addrB := mgrB.Addr()

	// Start A — listens, bootstraps to B
	cfgA := &config.Config{
		Node: config.NodeConfig{Name: "node-a"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			Bootstrap:         []string{addrB},
			MaxPeers:          15,
			ExchangeInterval:  fastExchange,
			DiscoveryInterval: fastDiscovery,
		},
	}
	mgrA, err := transport.NewManager(cfgA, nodeA.id, nodeA.hub, nil)
	if err != nil {
		t.Fatalf("manager A: %v", err)
	}
	nodeA.mgr = mgrA
	go nodeA.hub.Run()
	go mgrA.Start(nodeA.ctx)

	// Wait for A↔B connection
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgrA.PeerCount() >= 1 && mgrB.PeerCount() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgrA.PeerCount() == 0 || mgrB.PeerCount() == 0 {
		t.Fatalf("A↔B not connected: A=%d B=%d", mgrA.PeerCount(), mgrB.PeerCount())
	}

	// Start C — listens, bootstraps to B (does NOT know A)
	cfgC := &config.Config{
		Node: config.NodeConfig{Name: "node-c"},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			Bootstrap:         []string{addrB},
			MaxPeers:          15,
			ExchangeInterval:  fastExchange,
			DiscoveryInterval: fastDiscovery,
		},
	}
	mgrC, err := transport.NewManager(cfgC, nodeC.id, nodeC.hub, nil)
	if err != nil {
		t.Fatalf("manager C: %v", err)
	}
	nodeC.mgr = mgrC
	go nodeC.hub.Run()
	go mgrC.Start(nodeC.ctx)

	// Wait for full mesh: each node has 2 peers (A↔B, B↔C, A↔C via discovery)
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if mgrA.PeerCount() == 2 && mgrB.PeerCount() == 2 && mgrC.PeerCount() == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if mgrA.PeerCount() != 2 || mgrB.PeerCount() != 2 || mgrC.PeerCount() != 2 {
		t.Fatalf("mesh not formed: A=%d B=%d C=%d (want 2,2,2)",
			mgrA.PeerCount(), mgrB.PeerCount(), mgrC.PeerCount())
	}

	// Verify chat works across the full mesh: A → B and C
	nodeA.session = nodeA.hub.Join("alice")
	nodeB.session = nodeB.hub.Join("bob")
	nodeC.session = nodeC.hub.Join("carol")

	drainFor(nodeA.session.Send, 200*time.Millisecond)
	drainFor(nodeB.session.Send, 200*time.Millisecond)
	drainFor(nodeC.session.Send, 200*time.Millisecond)

	nodeA.hub.Broadcast(nodeA.session, "hello mesh")

	msgB := waitForMsg(nodeB.session.Send, 5*time.Second, "alice", "hello mesh")
	if msgB == "" {
		t.Error("node B did not receive message from A")
	}
	msgC := waitForMsg(nodeC.session.Send, 5*time.Second, "alice", "hello mesh")
	if msgC == "" {
		t.Error("node C did not receive message from A")
	}
}

// TestChainToMesh verifies that a linear chain A→B→C→D→E (bootstrap only)
// converges into a full mesh via PeerExchange, and that a message from A
// reaches E (which has no direct bootstrap connection to A).
func TestChainToMesh(t *testing.T) {
	const N = 5
	names := []string{"A", "B", "C", "D", "E"}
	nicks := []string{"alice", "bob", "carol", "dave", "eve"}

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
			t.Fatalf("identity %s: %v", names[i], err)
		}
		hub := partyline.NewHub("node-" + names[i])
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

	fastExchange := config.Duration{Duration: 1 * time.Second}
	fastDiscovery := config.Duration{Duration: 1 * time.Second}

	// Start nodes from E→A so each can bootstrap to the next.
	// E (index 4) has no bootstrap; D bootstraps to E; ... A bootstraps to B.
	addrs := make([]string, N)
	for i := N - 1; i >= 0; i-- {
		var bootstrap []string
		if i < N-1 {
			bootstrap = []string{addrs[i+1]}
		}
		cfg := &config.Config{
			Node: config.NodeConfig{Name: "node-" + names[i]},
			Network: config.NetworkConfig{
				Listen:            "127.0.0.1:0",
				MaxPeers:          15,
				Bootstrap:         bootstrap,
				ExchangeInterval:  fastExchange,
				DiscoveryInterval: fastDiscovery,
			},
		}
		mgr, err := transport.NewManager(cfg, nodes[i].id, nodes[i].hub, nil)
		if err != nil {
			t.Fatalf("manager %s: %v", names[i], err)
		}
		nodes[i].mgr = mgr
		go nodes[i].hub.Run()
		go mgr.Start(nodes[i].ctx)

		// Wait for listener
		deadline := time.Now().Add(5 * time.Second)
		for mgr.Addr() == "" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if mgr.Addr() == "" {
			t.Fatalf("node %s failed to start listener", names[i])
		}
		addrs[i] = mgr.Addr()
	}

	// Wait for the initial chain links to form: each adjacent pair connected.
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
	for i := 0; i < N; i++ {
		if nodes[i].mgr.PeerCount() == 0 {
			t.Fatalf("node %s has 0 peers (chain not formed)", names[i])
		}
	}

	// Wait for full mesh: each node has N-1 = 4 peers.
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		allFull := true
		for i := 0; i < N; i++ {
			if nodes[i].mgr.PeerCount() < N-1 {
				allFull = false
				break
			}
		}
		if allFull {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for i := 0; i < N; i++ {
		if nodes[i].mgr.PeerCount() != N-1 {
			counts := ""
			for j := 0; j < N; j++ {
				if j > 0 {
					counts += " "
				}
				counts += fmt.Sprintf("%s=%d", names[j], nodes[j].mgr.PeerCount())
			}
			t.Fatalf("mesh not formed: %s (want %d each)", counts, N-1)
		}
	}

	// Join sessions and drain join notifications
	for i := 0; i < N; i++ {
		nodes[i].session = nodes[i].hub.Join(nicks[i])
	}
	for i := 0; i < N; i++ {
		drainFor(nodes[i].session.Send, 200*time.Millisecond)
	}

	// A sends a message — verify E receives it
	nodes[0].hub.Broadcast(nodes[0].session, "hello from the edge")

	msg := waitForMsg(nodes[N-1].session.Send, 5*time.Second, nicks[0], "hello from the edge")
	if msg == "" {
		t.Fatal("node E did not receive message from A")
	}

	// Also verify all intermediate nodes received it
	for i := 1; i < N-1; i++ {
		msg := waitForMsg(nodes[i].session.Send, 5*time.Second, nicks[0], "hello from the edge")
		if msg == "" {
			t.Fatalf("node %s did not receive message from A", names[i])
		}
	}
}

// TestPeerRecordString verifies the Stringer implementation.
func TestPeerRecordString(t *testing.T) {
	rec := &transport.PeerRecord{
		NodeID:    "abcdef123456789",
		Addr:      "10.0.0.1:9000",
		Reachable: true,
		FailCount: 3,
	}
	s := rec.String()
	if s == "" {
		t.Fatal("expected non-empty string")
	}
	// Should contain truncated node ID
	expected := "PeerRecord{abcdef123456"
	if len(s) < len(expected) {
		t.Fatalf("string too short: %s", s)
	}
	if s[:len(expected)] != expected {
		t.Fatalf("unexpected prefix: %s", s)
	}

	// Short NodeID should not panic
	short := &transport.PeerRecord{NodeID: "abc"}
	if short.String() == "" {
		t.Fatal("short NodeID String() returned empty")
	}
}

// TestMaxPeersEnforcement verifies that MaxPeers is enforced.
// We set MaxPeers=1 on a center node and connect 2 spokes. Only 1 should succeed.
func TestMaxPeersEnforcement(t *testing.T) {
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

	center := makeNode("center")
	spoke1 := makeNode("spoke1")
	spoke2 := makeNode("spoke2")

	defer func() {
		for _, n := range []*testNode{&spoke2, &spoke1, &center} {
			if n.mgr != nil {
				n.mgr.Stop()
			}
			n.cancel()
			n.hub.Stop()
		}
	}()

	// Center with MaxPeers=1
	cfgCenter := &config.Config{
		Node:    config.NodeConfig{Name: "center"},
		Network: config.NetworkConfig{Listen: "127.0.0.1:0", MaxPeers: 1},
	}
	mgrCenter, err := transport.NewManager(cfgCenter, center.id, center.hub, nil)
	if err != nil {
		t.Fatalf("manager center: %v", err)
	}
	center.mgr = mgrCenter
	go center.hub.Run()
	go mgrCenter.Start(center.ctx)

	deadline := time.Now().Add(5 * time.Second)
	for mgrCenter.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	addr := mgrCenter.Addr()

	// Connect spoke1
	cfgS1 := &config.Config{
		Node:    config.NodeConfig{Name: "spoke1"},
		Network: config.NetworkConfig{Bootstrap: []string{addr}},
	}
	mgrS1, err := transport.NewManager(cfgS1, spoke1.id, spoke1.hub, nil)
	if err != nil {
		t.Fatalf("manager spoke1: %v", err)
	}
	spoke1.mgr = mgrS1
	go spoke1.hub.Run()
	go mgrS1.Start(spoke1.ctx)

	// Wait for spoke1 to connect
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgrCenter.PeerCount() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgrCenter.PeerCount() != 1 {
		t.Fatalf("center has %d peers, want 1", mgrCenter.PeerCount())
	}

	// Connect spoke2 — should be rejected by center
	cfgS2 := &config.Config{
		Node:    config.NodeConfig{Name: "spoke2"},
		Network: config.NetworkConfig{Bootstrap: []string{addr}},
	}
	mgrS2, err := transport.NewManager(cfgS2, spoke2.id, spoke2.hub, nil)
	if err != nil {
		t.Fatalf("manager spoke2: %v", err)
	}
	spoke2.mgr = mgrS2
	go spoke2.hub.Run()
	go mgrS2.Start(spoke2.ctx)

	// Give spoke2 time to attempt connection
	time.Sleep(2 * time.Second)

	// Center should still have exactly 1 peer
	if mgrCenter.PeerCount() != 1 {
		t.Fatalf("center has %d peers after MaxPeers enforcement, want 1", mgrCenter.PeerCount())
	}
}

// TestTenNodeMultiIP verifies discovery across 10 nodes each listening on a
// different loopback address (127.0.0.1 through 127.0.0.10). Only adjacent
// nodes know each other via bootstrap (chain topology). PeerExchange must
// propagate all addresses so every node discovers and dials the others,
// converging into a full 10-node mesh.
func TestTenNodeMultiIP(t *testing.T) {
	const N = 10

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

	fastExchange := config.Duration{Duration: 1 * time.Second}
	fastDiscovery := config.Duration{Duration: 1 * time.Second}

	// Start nodes from N-1 down to 0 so each can bootstrap to the next.
	// Each node listens on 127.0.0.(i+1):0
	addrs := make([]string, N)
	for i := N - 1; i >= 0; i-- {
		listenIP := fmt.Sprintf("127.0.0.%d", i+1)

		var bootstrap []string
		if i < N-1 {
			bootstrap = []string{addrs[i+1]}
		}

		cfg := &config.Config{
			Node: config.NodeConfig{Name: fmt.Sprintf("node-%d", i)},
			Network: config.NetworkConfig{
				Listen:            listenIP + ":0",
				MaxPeers:          15,
				Bootstrap:         bootstrap,
				ExchangeInterval:  fastExchange,
				DiscoveryInterval: fastDiscovery,
			},
		}
		mgr, err := transport.NewManager(cfg, nodes[i].id, nodes[i].hub, nil)
		if err != nil {
			t.Fatalf("manager node-%d: %v", i, err)
		}
		nodes[i].mgr = mgr
		go nodes[i].hub.Run()
		go mgr.Start(nodes[i].ctx)

		deadline := time.Now().Add(5 * time.Second)
		for mgr.Addr() == "" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if mgr.Addr() == "" {
			t.Fatalf("node-%d failed to start listener on %s", i, listenIP)
		}
		addrs[i] = mgr.Addr()
		t.Logf("node-%d listening on %s", i, addrs[i])
	}

	// Wait for chain links: every node has at least 1 peer
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
	for i := 0; i < N; i++ {
		if nodes[i].mgr.PeerCount() == 0 {
			t.Fatalf("node-%d has 0 peers (chain not formed)", i)
		}
	}

	// Wait for full mesh: each node has N-1 = 9 peers
	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		allFull := true
		for i := 0; i < N; i++ {
			if nodes[i].mgr.PeerCount() < N-1 {
				allFull = false
				break
			}
		}
		if allFull {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for i := 0; i < N; i++ {
		if nodes[i].mgr.PeerCount() != N-1 {
			counts := ""
			for j := 0; j < N; j++ {
				if j > 0 {
					counts += " "
				}
				counts += fmt.Sprintf("node-%d=%d", j, nodes[j].mgr.PeerCount())
			}
			t.Fatalf("mesh not formed: %s (want %d each)", counts, N-1)
		}
	}

	// Join sessions and drain
	nicks := []string{"alice", "bob", "carol", "dave", "eve", "frank", "grace", "heidi", "ivan", "judy"}
	for i := 0; i < N; i++ {
		nodes[i].session = nodes[i].hub.Join(nicks[i])
	}
	for i := 0; i < N; i++ {
		drainFor(nodes[i].session.Send, 200*time.Millisecond)
	}

	// Node 0 (127.0.0.1) sends a message — verify node 9 (127.0.0.10) receives it
	nodes[0].hub.Broadcast(nodes[0].session, "hello across loopbacks")

	msg := waitForMsg(nodes[N-1].session.Send, 5*time.Second, nicks[0], "hello across loopbacks")
	if msg == "" {
		t.Fatal("node-9 (127.0.0.10) did not receive message from node-0 (127.0.0.1)")
	}

	// Verify all other nodes received it too
	for i := 1; i < N-1; i++ {
		msg := waitForMsg(nodes[i].session.Send, 5*time.Second, nicks[0], "hello across loopbacks")
		if msg == "" {
			t.Fatalf("node-%d did not receive message from node-0", i)
		}
	}

	// Node 9 (127.0.0.10) sends back — verify node 0 (127.0.0.1) receives it
	for i := 0; i < N; i++ {
		drainFor(nodes[i].session.Send, 100*time.Millisecond)
	}
	nodes[N-1].hub.Broadcast(nodes[N-1].session, "reply from the edge")

	msg = waitForMsg(nodes[0].session.Send, 5*time.Second, nicks[N-1], "reply from the edge")
	if msg == "" {
		t.Fatal("node-0 (127.0.0.1) did not receive message from node-9 (127.0.0.10)")
	}
}


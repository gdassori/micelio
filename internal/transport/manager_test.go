package transport_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/partyline"
	"micelio/internal/transport"
)

// noDiscovery is an interval large enough to prevent auto-mesh during tests
// that rely on specific topologies (star, bridge).
var noDiscovery = config.Duration{Duration: 1 * time.Hour}

// drainFor discards messages from ch for duration d.
func drainFor(ch <-chan string, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ch:
		case <-timer.C:
			return
		}
	}
}

// waitForMsg reads from ch until a message containing all must substrings appears.
func waitForMsg(ch <-chan string, timeout time.Duration, must ...string) string {
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-ch:
			match := true
			for _, s := range must {
				if !strings.Contains(msg, s) {
					match = false
					break
				}
			}
			if match {
				return msg
			}
		case <-deadline:
			return ""
		}
	}
}

func TestTwoNodeChat(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	idA, err := identity.Load(dirA)
	if err != nil {
		t.Fatalf("identity A: %v", err)
	}
	idB, err := identity.Load(dirB)
	if err != nil {
		t.Fatalf("identity B: %v", err)
	}

	hubA := partyline.NewHub("node-a")
	hubB := partyline.NewHub("node-b")

	// Create manager B before hub.Run() to avoid data race on remoteSend.
	cfgB := &config.Config{
		Node:    config.NodeConfig{Name: "node-b", DataDir: dirB},
		Network: config.NetworkConfig{Listen: "127.0.0.1:0"},
	}
	mgrB, err := transport.NewManager(cfgB, idB, hubB, nil)
	if err != nil {
		t.Fatalf("manager B: %v", err)
	}

	go hubB.Run()
	defer hubB.Stop()

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go mgrB.Start(ctxB)

	// Wait for B's listener
	deadline := time.Now().Add(5 * time.Second)
	for mgrB.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if mgrB.Addr() == "" {
		t.Fatal("manager B failed to start listener")
	}

	// Create manager A before hubA.Run() to avoid data race on remoteSend.
	cfgA := &config.Config{
		Node:    config.NodeConfig{Name: "node-a", DataDir: dirA},
		Network: config.NetworkConfig{
			Listen:    "127.0.0.1:0",
			Bootstrap: []string{mgrB.Addr()},
		},
	}
	mgrA, err := transport.NewManager(cfgA, idA, hubA, nil)
	if err != nil {
		t.Fatalf("manager A: %v", err)
	}

	go hubA.Run()
	defer hubA.Stop()

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	go mgrA.Start(ctxA)

	// Wait for connection
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if mgrA.PeerCount() > 0 && mgrB.PeerCount() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgrA.PeerCount() == 0 || mgrB.PeerCount() == 0 {
		t.Fatalf("peers not connected: A=%d B=%d", mgrA.PeerCount(), mgrB.PeerCount())
	}

	// Join sessions to capture messages
	sessionA := hubA.Join("alice")
	sessionB := hubB.Join("bob")

	drainFor(sessionA.Send, 200*time.Millisecond)
	drainFor(sessionB.Send, 200*time.Millisecond)

	// A → B: alice sends a message
	hubA.Broadcast(sessionA, "hello from node A")

	if msg := waitForMsg(sessionB.Send, 5*time.Second, "alice", "hello from node A"); msg == "" {
		t.Fatal("timeout waiting for message on node B")
	}

	// B → A: bob sends a message
	hubB.Broadcast(sessionB, "hello from node B")

	if msg := waitForMsg(sessionA.Send, 5*time.Second, "bob", "hello from node B"); msg == "" {
		t.Fatal("timeout waiting for message on node A")
	}

	// Cleanup
	hubA.Leave(sessionA)
	hubB.Leave(sessionB)
	mgrA.Stop()
	mgrB.Stop()
}

// TestTenNodeStar creates a 10-node star topology where nodes 1-9 bootstrap
// to node 0. It verifies:
// 1. Center → spokes: a message from node 0 reaches all 9 other nodes.
// 2. Spoke → center: a message from node 7 reaches node 0.
// 3. Spoke → spoke via center: gossip relay delivers to other spokes.
func TestTenNodeStar(t *testing.T) {
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

	// Create identities and hubs (hubs NOT started yet — managers must be created first)
	for i := 0; i < N; i++ {
		dir := t.TempDir()
		id, err := identity.Load(dir)
		if err != nil {
			t.Fatalf("identity %d: %v", i, err)
		}
		hub := partyline.NewHub(fmt.Sprintf("node-%d", i))
		ctx, cancel := context.WithCancel(context.Background())
		nodes[i] = testNode{id: id, hub: hub, ctx: ctx, cancel: cancel}
	}

	// Cleanup (reverse order)
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

	// Create manager for node 0 (center) before hub.Run() to avoid data race on remoteSend.
	cfg0 := &config.Config{
		Node: config.NodeConfig{Name: "node-0", DataDir: nodes[0].id.NodeID},
		Network: config.NetworkConfig{
			Listen:            "127.0.0.1:0",
			ExchangeInterval:  noDiscovery,
			DiscoveryInterval: noDiscovery,
		},
	}
	mgr0, err := transport.NewManager(cfg0, nodes[0].id, nodes[0].hub, nil)
	if err != nil {
		t.Fatalf("manager 0: %v", err)
	}
	nodes[0].mgr = mgr0

	// Start center hub and listener
	go nodes[0].hub.Run()
	go mgr0.Start(nodes[0].ctx)

	// Wait for center listener
	deadline := time.Now().Add(5 * time.Second)
	for mgr0.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if mgr0.Addr() == "" {
		t.Fatal("node 0 failed to start listener")
	}
	centerAddr := mgr0.Addr()

	// Create managers for nodes 1-9 before starting their hubs.
	for i := 1; i < N; i++ {
		cfg := &config.Config{
			Node: config.NodeConfig{Name: fmt.Sprintf("node-%d", i)},
			Network: config.NetworkConfig{
				Listen:            "127.0.0.1:0",
				Bootstrap:         []string{centerAddr},
				ExchangeInterval:  noDiscovery,
				DiscoveryInterval: noDiscovery,
			},
		}
		mgr, err := transport.NewManager(cfg, nodes[i].id, nodes[i].hub, nil)
		if err != nil {
			t.Fatalf("manager %d: %v", i, err)
		}
		nodes[i].mgr = mgr
	}
	// Now start all spoke hubs and managers.
	for i := 1; i < N; i++ {
		go nodes[i].hub.Run()
		go nodes[i].mgr.Start(nodes[i].ctx)
	}

	// Wait until center has all 9 spokes connected
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if mgr0.PeerCount() == N-1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mgr0.PeerCount() != N-1 {
		t.Fatalf("center has %d peers, want %d", mgr0.PeerCount(), N-1)
	}

	// Join sessions on all nodes and drain join notifications
	for i := 0; i < N; i++ {
		nodes[i].session = nodes[i].hub.Join(fmt.Sprintf("user-%d", i))
	}
	for i := 0; i < N; i++ {
		drainFor(nodes[i].session.Send, 200*time.Millisecond)
	}

	// --- Test 1: center broadcasts, all spokes receive ---
	t.Run("center_to_spokes", func(t *testing.T) {
		nodes[0].hub.Broadcast(nodes[0].session, "hello from center")

		var wg sync.WaitGroup
		errs := make([]string, N)
		for i := 1; i < N; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				msg := waitForMsg(nodes[idx].session.Send, 5*time.Second, "user-0", "hello from center")
				if msg == "" {
					errs[idx] = fmt.Sprintf("node %d: timeout", idx)
				}
			}(i)
		}
		wg.Wait()
		for i := 1; i < N; i++ {
			if errs[i] != "" {
				t.Error(errs[i])
			}
		}
	})

	// Drain any residual messages
	for i := 0; i < N; i++ {
		drainFor(nodes[i].session.Send, 100*time.Millisecond)
	}

	// --- Test 2: spoke sends, center receives ---
	t.Run("spoke_to_center", func(t *testing.T) {
		nodes[7].hub.Broadcast(nodes[7].session, "ping from spoke-7")

		msg := waitForMsg(nodes[0].session.Send, 5*time.Second, "user-7", "ping from spoke-7")
		if msg == "" {
			t.Fatal("center did not receive message from spoke 7")
		}
	})

	// Drain residual
	for i := 0; i < N; i++ {
		drainFor(nodes[i].session.Send, 100*time.Millisecond)
	}

	// --- Test 3: spoke-to-spoke via gossip relay through center ---
	t.Run("spoke_gossip_relay", func(t *testing.T) {
		nodes[3].hub.Broadcast(nodes[3].session, "whisper from spoke-3")

		// Node 0 (center) should get it since node 3 is connected to it
		msg := waitForMsg(nodes[0].session.Send, 3*time.Second, "user-3", "whisper from spoke-3")
		if msg == "" {
			t.Fatal("center did not receive message from spoke 3")
		}

		// At least one other spoke should receive it via gossip relay through center.
		// With fanout=3 and 8 candidate spokes, any specific spoke has ~37.5% chance;
		// checking all ensures this test is not flaky.
		received := false
		for i := 1; i < N; i++ {
			if i == 3 {
				continue // sender
			}
			msg := waitForMsg(nodes[i].session.Send, 500*time.Millisecond, "user-3", "whisper from spoke-3")
			if msg != "" {
				received = true
			}
		}
		if !received {
			t.Fatal("no spoke received gossip-relayed message from spoke 3")
		}
	})
}

// TestOutboundBridge tests two isolated 3-node star networks (A and B) joined
// by a bridge node that has NO listen address — it only dials out to both
// centers. Topology:
//
//	Net A: a0 (center) ← a1, a2
//	Net B: b0 (center) ← b1, b2
//	Bridge: dials a0 AND b0 (no ingress)
//
// Verifies:
// 1. Bridge reaches both centers despite having no listen port.
// 2. Bridge sends a message → both centers receive it.
// 3. Center a0 sends → all nodes receive via gossip through bridge.
// 4. Center b0 sends → all nodes receive via gossip through bridge.
// 5. Cross-network gossip: a1's message reaches b1 via center → bridge → b0.
func TestOutboundBridge(t *testing.T) {
	const perNet = 3 // nodes per network (index 0 = center)

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
		// Hub NOT started here — must create Manager (SetRemoteSend) first.
		ctx, cancel := context.WithCancel(context.Background())
		return testNode{id: id, hub: hub, ctx: ctx, cancel: cancel}
	}

	// Create networks
	netA := make([]testNode, perNet)
	netB := make([]testNode, perNet)
	for i := 0; i < perNet; i++ {
		netA[i] = makeNode(fmt.Sprintf("a%d", i))
		netB[i] = makeNode(fmt.Sprintf("b%d", i))
	}
	bridge := makeNode("bridge")

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
	defer func() {
		cleanup(&bridge)
		for i := perNet - 1; i >= 0; i-- {
			cleanup(&netB[i])
			cleanup(&netA[i])
		}
	}()

	// startCenter creates manager, starts hub, starts listener, returns address.
	startCenter := func(n *testNode, name string) string {
		cfg := &config.Config{
			Node: config.NodeConfig{Name: name},
			Network: config.NetworkConfig{
				Listen:            "127.0.0.1:0",
				ExchangeInterval:  noDiscovery,
				DiscoveryInterval: noDiscovery,
			},
		}
		mgr, err := transport.NewManager(cfg, n.id, n.hub, nil)
		if err != nil {
			t.Fatalf("manager %s: %v", name, err)
		}
		n.mgr = mgr
		go n.hub.Run() // Start hub after manager (SetRemoteSend) to avoid data race.
		go mgr.Start(n.ctx)
		dl := time.Now().Add(5 * time.Second)
		for mgr.Addr() == "" && time.Now().Before(dl) {
			time.Sleep(10 * time.Millisecond)
		}
		if mgr.Addr() == "" {
			t.Fatalf("%s failed to listen", name)
		}
		return mgr.Addr()
	}

	// startSpoke creates manager, starts hub, bootstraps to center.
	startSpoke := func(n *testNode, name, centerAddr string) {
		cfg := &config.Config{
			Node: config.NodeConfig{Name: name},
			Network: config.NetworkConfig{
				Listen:            "127.0.0.1:0",
				Bootstrap:         []string{centerAddr},
				ExchangeInterval:  noDiscovery,
				DiscoveryInterval: noDiscovery,
			},
		}
		mgr, err := transport.NewManager(cfg, n.id, n.hub, nil)
		if err != nil {
			t.Fatalf("manager %s: %v", name, err)
		}
		n.mgr = mgr
		go n.hub.Run() // Start hub after manager (SetRemoteSend) to avoid data race.
		go mgr.Start(n.ctx)
	}

	// Boot Net A
	addrA := startCenter(&netA[0], "a0")
	for i := 1; i < perNet; i++ {
		startSpoke(&netA[i], fmt.Sprintf("a%d", i), addrA)
	}

	// Boot Net B
	addrB := startCenter(&netB[0], "b0")
	for i := 1; i < perNet; i++ {
		startSpoke(&netB[i], fmt.Sprintf("b%d", i), addrB)
	}

	// Wait for both networks to form
	waitPeers := func(mgr *transport.Manager, want int, name string) {
		dl := time.Now().Add(10 * time.Second)
		for time.Now().Before(dl) {
			if mgr.PeerCount() >= want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s has %d peers, want >= %d", name, mgr.PeerCount(), want)
	}
	waitPeers(netA[0].mgr, perNet-1, "a0")
	waitPeers(netB[0].mgr, perNet-1, "b0")

	// Start bridge — NO listen address, dials both centers
	bridgeCfg := &config.Config{
		Node: config.NodeConfig{Name: "bridge"},
		Network: config.NetworkConfig{
			Bootstrap:         []string{addrA, addrB},
			ExchangeInterval:  noDiscovery,
			DiscoveryInterval: noDiscovery,
		},
	}
	mgrBridge, err := transport.NewManager(bridgeCfg, bridge.id, bridge.hub, nil)
	if err != nil {
		t.Fatalf("manager bridge: %v", err)
	}
	bridge.mgr = mgrBridge
	go bridge.hub.Run() // Start hub after manager to avoid data race.
	go mgrBridge.Start(bridge.ctx)

	// Bridge should connect to both centers
	waitPeers(mgrBridge, 2, "bridge")
	// Centers should now have one extra peer
	waitPeers(netA[0].mgr, perNet, "a0+bridge")
	waitPeers(netB[0].mgr, perNet, "b0+bridge")

	if mgrBridge.Addr() != "" {
		t.Fatalf("bridge should have no listener, got %s", mgrBridge.Addr())
	}

	// Join sessions on all nodes
	for i := 0; i < perNet; i++ {
		netA[i].session = netA[i].hub.Join(fmt.Sprintf("userA%d", i))
		netB[i].session = netB[i].hub.Join(fmt.Sprintf("userB%d", i))
	}
	bridge.session = bridge.hub.Join("bridge-op")

	drainAll := func() {
		for i := 0; i < perNet; i++ {
			drainFor(netA[i].session.Send, 100*time.Millisecond)
			drainFor(netB[i].session.Send, 100*time.Millisecond)
		}
		drainFor(bridge.session.Send, 100*time.Millisecond)
	}
	drainAll()

	// --- Test 1: bridge sends → both centers receive ---
	t.Run("bridge_to_both_centers", func(t *testing.T) {
		bridge.hub.Broadcast(bridge.session, "hello from bridge")

		msgA := waitForMsg(netA[0].session.Send, 5*time.Second, "bridge-op", "hello from bridge")
		if msgA == "" {
			t.Error("center a0 did not receive bridge message")
		}
		msgB := waitForMsg(netB[0].session.Send, 5*time.Second, "bridge-op", "hello from bridge")
		if msgB == "" {
			t.Error("center b0 did not receive bridge message")
		}
	})

	drainAll()

	// --- Test 2: a0 sends → all nodes receive via gossip through bridge ---
	t.Run("netA_center_broadcast", func(t *testing.T) {
		netA[0].hub.Broadcast(netA[0].session, "msg from a0")

		// a1, a2, bridge should receive directly
		for _, pair := range []struct {
			name string
			ch   <-chan string
		}{
			{"a1", netA[1].session.Send},
			{"a2", netA[2].session.Send},
			{"bridge", bridge.session.Send},
		} {
			if waitForMsg(pair.ch, 5*time.Second, "userA0", "msg from a0") == "" {
				t.Errorf("%s did not receive a0's message", pair.name)
			}
		}

		// b0 should receive via gossip through the bridge
		if waitForMsg(netB[0].session.Send, 5*time.Second, "userA0", "msg from a0") == "" {
			t.Error("b0 did not receive a0's message via gossip through bridge")
		}
	})

	drainAll()

	// --- Test 3: b0 sends → all nodes receive via gossip through bridge ---
	t.Run("netB_center_broadcast", func(t *testing.T) {
		netB[0].hub.Broadcast(netB[0].session, "msg from b0")

		for _, pair := range []struct {
			name string
			ch   <-chan string
		}{
			{"b1", netB[1].session.Send},
			{"b2", netB[2].session.Send},
			{"bridge", bridge.session.Send},
		} {
			if waitForMsg(pair.ch, 5*time.Second, "userB0", "msg from b0") == "" {
				t.Errorf("%s did not receive b0's message", pair.name)
			}
		}

		// a0 should receive via gossip through the bridge
		if waitForMsg(netA[0].session.Send, 5*time.Second, "userB0", "msg from b0") == "" {
			t.Error("a0 did not receive b0's message via gossip through bridge")
		}
	})

	drainAll()

	// --- Test 4: a1 sends → gossip relay propagates through a0 → bridge → Net B ---
	t.Run("spoke_gossip_relay", func(t *testing.T) {
		netA[1].hub.Broadcast(netA[1].session, "spoke a1 whisper")

		if waitForMsg(netA[0].session.Send, 3*time.Second, "userA1", "spoke a1 whisper") == "" {
			t.Error("a0 did not receive a1's message")
		}

		// Bridge receives via gossip relay through a0
		if waitForMsg(bridge.session.Send, 5*time.Second, "userA1", "spoke a1 whisper") == "" {
			t.Error("bridge did not receive a1's message via gossip relay")
		}

		// b0 receives via gossip relay through bridge
		if waitForMsg(netB[0].session.Send, 5*time.Second, "userA1", "spoke a1 whisper") == "" {
			t.Error("b0 did not receive a1's message via gossip relay through bridge")
		}
	})
}

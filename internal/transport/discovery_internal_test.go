package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/partyline"

	pb "micelio/pkg/proto"
)

// makeTestManager creates a minimal Manager for unit-testing discovery internals.
func makeTestManager(t *testing.T) *Manager {
	t.Helper()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	hub := partyline.NewHub("test")
	cfg := &config.Config{
		Node:    config.NodeConfig{Name: "test"},
		Network: config.NetworkConfig{},
	}
	mgr, err := NewManager(cfg, id, hub, nil)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	return mgr
}

// validPeerInfo creates a PeerInfo with correctly derived node_id.
func validPeerInfo(t *testing.T, addr string) *pb.PeerInfo {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	hash := sha256.Sum256(pub)
	return &pb.PeerInfo{
		NodeId:        hex.EncodeToString(hash[:]),
		Addr:          addr,
		Reachable:     true,
		Ed25519Pubkey: pub,
		LastSeen:      1000,
	}
}

func TestMergeRejectsMismatchedNodeID(t *testing.T) {
	mgr := makeTestManager(t)

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	info := &pb.PeerInfo{
		NodeId:        "0000000000000000000000000000000000000000000000000000000000000000",
		Addr:          "10.0.0.1:4000",
		Reachable:     true,
		Ed25519Pubkey: pub,
		LastSeen:      1000,
	}

	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info}, "sender")

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.knownPeers) != 0 {
		t.Fatalf("expected 0 known peers after mismatched node_id, got %d", len(mgr.knownPeers))
	}
}

func TestMergeRejectsWildcardAddr(t *testing.T) {
	mgr := makeTestManager(t)

	wildcards := []string{
		"0.0.0.0:4000",
		"[::]:4000",
		":4000",
	}
	for _, addr := range wildcards {
		info := validPeerInfo(t, addr)
		mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info}, "sender")
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.knownPeers) != 0 {
		t.Fatalf("expected 0 known peers after wildcard addrs, got %d", len(mgr.knownPeers))
	}
}

func TestMergeRejectsEmptyAddr(t *testing.T) {
	mgr := makeTestManager(t)

	info := validPeerInfo(t, "")
	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info}, "sender")

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.knownPeers) != 0 {
		t.Fatalf("expected 0 known peers after empty addr, got %d", len(mgr.knownPeers))
	}
}

func TestMergeRejectsNonReachable(t *testing.T) {
	mgr := makeTestManager(t)

	info := validPeerInfo(t, "10.0.0.1:4000")
	info.Reachable = false
	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info}, "sender")

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.knownPeers) != 0 {
		t.Fatalf("expected 0 known peers after non-reachable, got %d", len(mgr.knownPeers))
	}
}

func TestMergeRejectsInvalidPubkeySize(t *testing.T) {
	mgr := makeTestManager(t)

	info := &pb.PeerInfo{
		NodeId:        "abcd",
		Addr:          "10.0.0.1:4000",
		Reachable:     true,
		Ed25519Pubkey: []byte("tooshort"),
		LastSeen:      1000,
	}
	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info}, "sender")

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.knownPeers) != 0 {
		t.Fatalf("expected 0 known peers after invalid pubkey size, got %d", len(mgr.knownPeers))
	}
}

func TestMergeSkipsSelf(t *testing.T) {
	mgr := makeTestManager(t)

	// Build a PeerInfo with this manager's own node ID and key
	info := &pb.PeerInfo{
		NodeId:        mgr.id.NodeID,
		Addr:          "10.0.0.1:4000",
		Reachable:     true,
		Ed25519Pubkey: mgr.id.PublicKey,
		LastSeen:      1000,
	}
	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info}, "sender")

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.knownPeers) != 0 {
		t.Fatalf("expected 0 known peers after self-info, got %d", len(mgr.knownPeers))
	}
}

func TestMergeAcceptsValidPeer(t *testing.T) {
	mgr := makeTestManager(t)

	info := validPeerInfo(t, "10.0.0.1:4000")
	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info}, "sender")

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.knownPeers) != 1 {
		t.Fatalf("expected 1 known peer, got %d", len(mgr.knownPeers))
	}
	rec := mgr.knownPeers[info.NodeId]
	if rec == nil {
		t.Fatal("peer not found in knownPeers")
	}
	if rec.Addr != "10.0.0.1:4000" {
		t.Fatalf("expected addr 10.0.0.1:4000, got %s", rec.Addr)
	}
}

func TestMergeUpdatesOnlyIfNewer(t *testing.T) {
	mgr := makeTestManager(t)

	info := validPeerInfo(t, "10.0.0.1:4000")
	info.LastSeen = 2000
	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info}, "sender")

	// Send older update — should NOT change addr
	info2 := &pb.PeerInfo{
		NodeId:        info.NodeId,
		Addr:          "10.0.0.2:5000",
		Reachable:     true,
		Ed25519Pubkey: info.Ed25519Pubkey,
		LastSeen:      1000, // older
	}
	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info2}, "sender")

	func() {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		rec := mgr.knownPeers[info.NodeId]
		if rec.Addr != "10.0.0.1:4000" {
			t.Fatalf("addr changed to %s despite older LastSeen", rec.Addr)
		}
	}()

	// Send newer update — SHOULD change addr
	info3 := &pb.PeerInfo{
		NodeId:        info.NodeId,
		Addr:          "10.0.0.3:6000",
		Reachable:     true,
		Ed25519Pubkey: info.Ed25519Pubkey,
		LastSeen:      3000, // newer
	}
	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info3}, "sender")

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	rec := mgr.knownPeers[info.NodeId]
	if rec.Addr != "10.0.0.3:6000" {
		t.Fatalf("addr not updated to 10.0.0.3:6000, got %s", rec.Addr)
	}
}

func TestBuildPeerInfoExcludesWildcard(t *testing.T) {
	mgr := makeTestManager(t)

	// Manually insert a known peer with a wildcard addr
	info := validPeerInfo(t, "10.0.0.1:4000")
	mgr.mergeDiscoveredPeers([]*pb.PeerInfo{info}, "sender")

	wildcardInfo := validPeerInfo(t, "0.0.0.0:4000")
	// Force-insert with wildcard addr (bypassing merge validation)
	mgr.mu.Lock()
	mgr.knownPeers[wildcardInfo.NodeId] = &PeerRecord{
		NodeID:    wildcardInfo.NodeId,
		Addr:      "0.0.0.0:4000",
		Reachable: true,
		PublicKey: "aa",
		LastSeen:  1000,
	}
	mgr.mu.Unlock()

	infos := mgr.buildPeerInfoList("nobody")
	// Should include the valid peer but NOT the wildcard peer (self may also be excluded)
	for _, pi := range infos {
		if pi.Addr == "0.0.0.0:4000" {
			t.Fatal("buildPeerInfoList should exclude wildcard addresses")
		}
	}
}

func TestIsInvalidPeerAddr(t *testing.T) {
	cases := []struct {
		addr    string
		invalid bool
	}{
		{"0.0.0.0:4000", true},
		{"[::]:4000", true},
		{":4000", true},
		{"not-a-hostport", true},
		{"just-a-host", true},
		{"10.0.0.1:4000", false},
		{"127.0.0.1:4000", false},
		{"example.com:4000", false},
	}
	for _, tc := range cases {
		got := isInvalidPeerAddr(tc.addr)
		if got != tc.invalid {
			t.Errorf("isInvalidPeerAddr(%q) = %v, want %v", tc.addr, got, tc.invalid)
		}
	}
}

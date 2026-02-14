package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/logging"
	"micelio/internal/partyline"
	boltstore "micelio/internal/store/bolt"

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
	capture := logging.CaptureForTest()
	defer capture.Restore()

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

	if !capture.Has(slog.LevelWarn, "ignoring peer: mismatched node_id") {
		t.Error("expected WARN log: ignoring peer: mismatched node_id")
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
	capture := logging.CaptureForTest()
	defer capture.Restore()

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

	if capture.Count(slog.LevelWarn) != 0 {
		t.Errorf("unexpected WARN logs: %d", capture.Count(slog.LevelWarn))
	}
	if capture.Count(slog.LevelError) != 0 {
		t.Errorf("unexpected ERROR logs: %d", capture.Count(slog.LevelError))
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

// --- PeerRecord.backoff tests ---

func TestBackoffZeroFails(t *testing.T) {
	rec := &PeerRecord{FailCount: 0}
	if d := rec.backoff(); d != 0 {
		t.Fatalf("expected 0 backoff for 0 fails, got %v", d)
	}
}

func TestBackoffExponential(t *testing.T) {
	cases := []struct {
		failCount int
		wantSecs  float64
	}{
		{1, 1},   // 2^0 = 1
		{2, 2},   // 2^1 = 2
		{3, 4},   // 2^2 = 4
		{4, 8},   // 2^3 = 8
		{5, 16},  // 2^4 = 16
		{6, 30},  // 2^5 = 32, capped at 30
		{10, 30}, // capped at 30
	}
	for _, tc := range cases {
		rec := &PeerRecord{FailCount: tc.failCount}
		got := rec.backoff()
		want := time.Duration(tc.wantSecs) * time.Second
		if got != want {
			t.Errorf("backoff(fails=%d) = %v, want %v", tc.failCount, got, want)
		}
	}
}

func TestBackoffNegativeFails(t *testing.T) {
	rec := &PeerRecord{FailCount: -1}
	if d := rec.backoff(); d != 0 {
		t.Fatalf("expected 0 backoff for negative fails, got %v", d)
	}
}

// --- safeLastSeen tests ---

func TestSafeLastSeenNormal(t *testing.T) {
	got := safeLastSeen(1000)
	if got != 1000 {
		t.Fatalf("expected 1000, got %d", got)
	}
}

func TestSafeLastSeenOverflow(t *testing.T) {
	got := safeLastSeen(math.MaxUint64)
	if got != math.MaxInt64 {
		t.Fatalf("expected MaxInt64, got %d", got)
	}
}

func TestSafeLastSeenBoundary(t *testing.T) {
	// Exactly MaxInt64 should pass through
	got := safeLastSeen(uint64(math.MaxInt64))
	if got != math.MaxInt64 {
		t.Fatalf("expected MaxInt64, got %d", got)
	}
	// MaxInt64 + 1 should be clamped
	got = safeLastSeen(uint64(math.MaxInt64) + 1)
	if got != math.MaxInt64 {
		t.Fatalf("expected MaxInt64 for overflow, got %d", got)
	}
}

// --- saveKnownPeer + loadKnownPeers round-trip ---

func TestSaveAndLoadKnownPeers(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	dir := t.TempDir()

	// Create identity and store
	id, err := identity.Load(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	hub := partyline.NewHub("test")
	st, err := boltstore.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	cfg := &config.Config{
		Node:    config.NodeConfig{Name: "test"},
		Network: config.NetworkConfig{},
	}

	// Create manager with store, save a peer
	mgr, err := NewManager(cfg, id, hub, st)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	// Generate a valid peer key
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(pub)
	nodeID := hex.EncodeToString(hash[:])

	rec := &PeerRecord{
		NodeID:    nodeID,
		Addr:      "10.0.0.1:4000",
		Reachable: true,
		PublicKey: hex.EncodeToString(pub),
		LastSeen:  time.Now().Unix(),
	}
	mgr.saveKnownPeer(rec)

	// Close the store, reopen, and create a new manager to test loadKnownPeers
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st2, err := boltstore.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()

	mgr2, err := NewManager(cfg, id, hub, st2)
	if err != nil {
		t.Fatalf("manager2: %v", err)
	}

	mgr2.mu.Lock()
	loaded, ok := mgr2.knownPeers[nodeID]
	mgr2.mu.Unlock()

	if !ok {
		t.Fatal("expected peer to be loaded from store")
	}
	if loaded.Addr != "10.0.0.1:4000" {
		t.Fatalf("expected addr 10.0.0.1:4000, got %s", loaded.Addr)
	}
	if loaded.PublicKey != hex.EncodeToString(pub) {
		t.Fatal("public key mismatch after load")
	}

	if !capture.Has(slog.LevelInfo, "loaded known peers") {
		t.Error("expected INFO log: loaded known peers")
	}
	if capture.Count(slog.LevelWarn) != 0 {
		t.Errorf("unexpected WARN logs: %d", capture.Count(slog.LevelWarn))
	}
}

func TestSaveKnownPeerNilStore(t *testing.T) {
	mgr := makeTestManager(t) // nil store
	rec := &PeerRecord{NodeID: "test", Addr: "1.2.3.4:5000"}
	// Should not panic
	mgr.saveKnownPeer(rec)
}

func TestLoadKnownPeersSkipsInvalidRecords(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	dir := t.TempDir()
	id, err := identity.Load(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	hub := partyline.NewHub("test")
	st, err := boltstore.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Manually insert invalid records into the store
	mustSet := func(key string, val []byte) {
		t.Helper()
		if err := st.Set(peersBucket, []byte(key), val); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}

	// 1. Invalid JSON
	mustSet("bad-json", []byte("not-json{"))

	// 2. Empty NodeID
	emptyID, _ := json.Marshal(PeerRecord{Addr: "1.2.3.4:5000"})
	mustSet("empty-id", emptyID)

	// 3. Invalid addr (wildcard)
	wildcard, _ := json.Marshal(PeerRecord{NodeID: "abc123", Addr: "0.0.0.0:4000"})
	mustSet("wildcard", wildcard)

	// 4. Invalid pubkey (bad hex)
	badHex, _ := json.Marshal(PeerRecord{NodeID: "abc123", Addr: "10.0.0.1:4000", PublicKey: "not-hex!!"})
	mustSet("bad-hex", badHex)

	// 5. Invalid pubkey (wrong length)
	shortKey, _ := json.Marshal(PeerRecord{NodeID: "abc123", Addr: "10.0.0.1:4000", PublicKey: hex.EncodeToString([]byte("short"))})
	mustSet("short-key", shortKey)

	// 6. Pubkey hash doesn't match NodeID
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	mismatch, _ := json.Marshal(PeerRecord{
		NodeID:    "0000000000000000000000000000000000000000000000000000000000000000",
		Addr:      "10.0.0.1:4000",
		PublicKey: hex.EncodeToString(pub),
	})
	mustSet("mismatch", mismatch)

	// 7. Valid record (no pubkey, just addr — should be accepted)
	validNoPub, _ := json.Marshal(PeerRecord{NodeID: "valid-no-pub", Addr: "10.0.0.2:4000"})
	mustSet("valid-no-pub", validNoPub)

	cfg := &config.Config{
		Node:    config.NodeConfig{Name: "test"},
		Network: config.NetworkConfig{},
	}
	mgr, err := NewManager(cfg, id, hub, st)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	// Only the valid record without pubkey should have been loaded
	if len(mgr.knownPeers) != 1 {
		t.Fatalf("expected 1 valid peer loaded, got %d", len(mgr.knownPeers))
	}
	if _, ok := mgr.knownPeers["valid-no-pub"]; !ok {
		t.Fatal("expected valid-no-pub to be loaded")
	}

	// Should have WARN logs for each invalid record
	if !capture.Has(slog.LevelWarn, "unmarshal peer record") {
		t.Error("expected WARN log: unmarshal peer record")
	}
	if !capture.Has(slog.LevelWarn, "skipping peer: empty NodeID") {
		t.Error("expected WARN log: skipping peer: empty NodeID")
	}
	if !capture.Has(slog.LevelWarn, "skipping peer: invalid addr") {
		t.Error("expected WARN log: skipping peer: invalid addr")
	}
	if !capture.Has(slog.LevelWarn, "skipping peer: invalid pubkey encoding") {
		t.Error("expected WARN log: skipping peer: invalid pubkey encoding")
	}
	if !capture.Has(slog.LevelWarn, "skipping peer: invalid pubkey length") {
		t.Error("expected WARN log: skipping peer: invalid pubkey length")
	}
	if !capture.Has(slog.LevelWarn, "skipping peer: mismatched pubkey") {
		t.Error("expected WARN log: skipping peer: mismatched pubkey")
	}
	if !capture.Has(slog.LevelInfo, "loaded known peers") {
		t.Error("expected INFO log: loaded known peers")
	}
}

func TestLoadKnownPeersNilStore(t *testing.T) {
	mgr := makeTestManager(t) // nil store
	// loadKnownPeers is called during NewManager; just verify it didn't crash
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.knownPeers) != 0 {
		t.Fatalf("expected 0 peers with nil store, got %d", len(mgr.knownPeers))
	}
}

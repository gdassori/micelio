package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"micelio/internal/config"
	"micelio/internal/identity"
	"micelio/internal/logging"
	"micelio/internal/partyline"
	"micelio/internal/state"
	"micelio/internal/store"
	boltstore "micelio/internal/store/bolt"
	pb "micelio/pkg/proto"
)

// testManager creates a minimal Manager with real stateMap and optional store.
func testManager(t *testing.T, st store.Store) *Manager {
	t.Helper()
	dir := t.TempDir()
	id, err := identity.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub := partyline.NewHub("test")
	cfg := &config.Config{
		Node:    config.NodeConfig{Name: "test"},
		Network: config.NetworkConfig{},
	}
	mgr, err := NewManager(cfg, id, hub, st)
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

// testPeer creates a minimal Peer with a sendCh for testing.
func testPeer(nodeID string) *Peer {
	return &Peer{
		NodeID: nodeID,
		sendCh: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
}

// removePeers removes all test peers from the manager so Stop() doesn't
// try to close nil connections.
func removePeers(mgr *Manager) {
	mgr.mu.Lock()
	mgr.peers = make(map[string]*Peer)
	mgr.mu.Unlock()
}

// --- enqueuePersist tests ---

func TestEnqueuePersist(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	entry := state.Entry{Key: "test/key", Value: []byte("val"), LamportTs: 1}
	mgr.enqueuePersist(entry)

	select {
	case got := <-mgr.persistCh:
		if got.Key != entry.Key {
			t.Fatalf("got key %q, want %q", got.Key, entry.Key)
		}
	case <-time.After(time.Second):
		t.Fatal("entry not enqueued")
	}
}

func TestEnqueuePersistBufferFull(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	mgr := testManager(t, nil)
	defer mgr.Stop()

	// Fill the buffer
	for range cap(mgr.persistCh) {
		mgr.persistCh <- state.Entry{Key: "fill"}
	}

	// This should be dropped with a warning
	mgr.enqueuePersist(state.Entry{Key: "dropped"})

	if capture.Count(slog.LevelWarn) == 0 {
		t.Fatal("expected warning log for full buffer")
	}
}

// --- persistLoop tests ---

func TestPersistLoop(t *testing.T) {
	dir := t.TempDir()
	st, err := boltstore.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	mgr := testManager(t, st)

	// Start persistLoop in background
	go mgr.persistLoop()

	// Create a signed entry
	entry, _, err := mgr.stateMap.Set("color", []byte("blue"))
	if err != nil {
		t.Fatal(err)
	}
	mgr.enqueuePersist(entry)

	// Give persistLoop time to process
	time.Sleep(200 * time.Millisecond)

	// Stop the manager to drain and exit persistLoop
	mgr.Stop()

	// Verify entry is in the store
	got, err := st.Get([]byte("state"), []byte(entry.Key))
	if err != nil {
		t.Fatalf("Get from store: %v", err)
	}
	if got == nil {
		t.Fatal("entry not persisted to store")
	}
}

func TestPersistLoopDrainsOnShutdown(t *testing.T) {
	dir := t.TempDir()
	st, err := boltstore.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	mgr := testManager(t, st)

	// Enqueue entries BEFORE starting persistLoop
	e1, _, _ := mgr.stateMap.Set("k1", []byte("v1"))
	e2, _, _ := mgr.stateMap.Set("k2", []byte("v2"))
	mgr.persistCh <- e1
	mgr.persistCh <- e2

	// Start persistLoop and immediately stop
	go mgr.persistLoop()
	time.Sleep(50 * time.Millisecond)
	mgr.Stop()
	time.Sleep(100 * time.Millisecond)

	// Verify both entries were drained
	for _, key := range []string{e1.Key, e2.Key} {
		got, err := st.Get([]byte("state"), []byte(key))
		if err != nil {
			t.Fatalf("Get(%q): %v", key, err)
		}
		if got == nil {
			t.Fatalf("entry %q not drained on shutdown", key)
		}
	}
}

// --- gcLoop tests ---

func TestGcLoop(t *testing.T) {
	dir := t.TempDir()
	st, err := boltstore.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	mgr := testManager(t, st)
	// Override GC config for fast test
	mgr.cfg.State.TombstoneTTL = config.Duration{Duration: 0}
	mgr.cfg.State.GCInterval = config.Duration{Duration: 50 * time.Millisecond}

	// Create a tombstone and persist it
	e, _, _ := mgr.stateMap.Set("temp", []byte("val"))
	state.PersistEntry(st, e)
	del, _, _ := mgr.stateMap.Delete("temp")
	state.PersistEntry(st, del)

	// Create a live entry
	live, _, _ := mgr.stateMap.Set("live", []byte("alive"))
	state.PersistEntry(st, live)

	ctx, cancel := context.WithCancel(context.Background())
	go mgr.gcLoop(ctx)

	// Wait for GC to run at least once
	time.Sleep(200 * time.Millisecond)
	cancel()

	// Tombstone should be removed from state map
	tombKey := mgr.id.NodeID + "/temp"
	snap := mgr.stateMap.Snapshot()
	for _, e := range snap {
		if e.Key == tombKey {
			t.Fatal("tombstone should have been GC'd from state map")
		}
	}

	// Tombstone should be removed from store
	got, err := st.Get([]byte("state"), []byte(tombKey))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatal("tombstone should have been deleted from store")
	}

	// Live entry should still exist
	liveKey := mgr.id.NodeID + "/live"
	if _, ok := mgr.stateMap.Get(liveKey); !ok {
		t.Fatal("live entry should survive GC")
	}
}

func TestGcLoopExitsOnCancel(t *testing.T) {
	mgr := testManager(t, nil)
	mgr.cfg.State.TombstoneTTL = config.Duration{Duration: time.Hour}
	mgr.cfg.State.GCInterval = config.Duration{Duration: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.gcLoop(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gcLoop did not exit on context cancel")
	}
}

func TestGcLoopExitsOnManagerDone(t *testing.T) {
	mgr := testManager(t, nil)
	mgr.cfg.State.TombstoneTTL = config.Duration{Duration: time.Hour}
	mgr.cfg.State.GCInterval = config.Duration{Duration: time.Hour}

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		mgr.gcLoop(ctx)
		close(done)
	}()

	mgr.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gcLoop did not exit on manager stop")
	}
}

// --- sendStateSyncRequestTo tests ---

func TestSendStateSyncRequestToNilStateMap(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	mgr.stateMap = nil
	p := testPeer("remote-node")

	// Should not panic
	mgr.sendStateSyncRequestTo(p)

	select {
	case <-p.sendCh:
		t.Fatal("should not send when stateMap is nil")
	default:
	}
}

func TestSendStateSyncRequestTo(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	// Set some state to create a non-empty digest
	mgr.stateMap.Set("key1", []byte("val1")) //nolint:errcheck
	mgr.stateMap.Set("key2", []byte("val2")) //nolint:errcheck

	p := testPeer("remote-node")
	mgr.sendStateSyncRequestTo(p)

	select {
	case data := <-p.sendCh:
		if len(data) == 0 {
			t.Fatal("empty envelope sent")
		}
	case <-time.After(time.Second):
		t.Fatal("no message sent to peer")
	}
}

func TestSendStateSyncRequestToBufferFull(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	mgr := testManager(t, nil)
	defer mgr.Stop()

	p := testPeer("remote-node")
	// Fill the send buffer
	for range cap(p.sendCh) {
		p.sendCh <- []byte("fill")
	}

	mgr.sendStateSyncRequestTo(p)

	if capture.Count(slog.LevelWarn) == 0 {
		t.Fatal("expected warning for full send buffer")
	}
}

// --- sendStateSyncResponseTo tests ---

func TestSendStateSyncResponseToNilStateMap(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	mgr.stateMap = nil
	p := testPeer("remote-node")

	mgr.sendStateSyncResponseTo(p, nil)

	select {
	case <-p.sendCh:
		t.Fatal("should not send when stateMap is nil")
	default:
	}
}

func TestSendStateSyncResponseToEmptyDelta(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	// State map is empty, so delta will be empty
	p := testPeer("remote-node")
	mgr.sendStateSyncResponseTo(p, nil)

	select {
	case <-p.sendCh:
		t.Fatal("should not send empty response")
	default:
	}
}

func TestSendStateSyncResponseToFullDelta(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	mgr.stateMap.Set("a", []byte("1")) //nolint:errcheck
	mgr.stateMap.Set("b", []byte("2")) //nolint:errcheck

	p := testPeer("remote-node")
	// nil known = full dump
	mgr.sendStateSyncResponseTo(p, nil)

	select {
	case data := <-p.sendCh:
		if len(data) == 0 {
			t.Fatal("empty envelope sent")
		}
	case <-time.After(time.Second):
		t.Fatal("no response sent")
	}
}

func TestSendStateSyncResponseToPartialDigest(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	mgr.stateMap.Set("a", []byte("1")) //nolint:errcheck
	mgr.stateMap.Set("b", []byte("2")) //nolint:errcheck

	p := testPeer("remote-node")
	// Peer already knows everything from this node's namespace at ts=100
	known := map[string]uint64{mgr.id.NodeID: 100}
	mgr.sendStateSyncResponseTo(p, known)

	// Delta should be empty (peer already has everything)
	select {
	case <-p.sendCh:
		t.Fatal("should not send when peer is up to date")
	default:
	}
}

func TestSendStateSyncResponseToBufferFull(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	mgr := testManager(t, nil)
	defer mgr.Stop()

	mgr.stateMap.Set("a", []byte("1")) //nolint:errcheck

	p := testPeer("remote-node")
	for range cap(p.sendCh) {
		p.sendCh <- []byte("fill")
	}

	mgr.sendStateSyncResponseTo(p, nil)

	if capture.Count(slog.LevelWarn) == 0 {
		t.Fatal("expected warning for full send buffer")
	}
}

func TestSendStateSyncResponseTruncation(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	// Create many entries from different "nodes" to exceed maxStateSyncEntries
	_, priv, _ := ed25519.GenerateKey(nil)
	pub := priv.Public().(ed25519.PublicKey)
	hash := sha256.Sum256(pub)
	nodeID := hex.EncodeToString(hash[:])

	sm := state.NewMap(nodeID, priv)
	for i := range maxStateSyncEntries + 10 {
		sm.Set("k"+string(rune(i)), []byte("v")) //nolint:errcheck
	}

	mgr := testManager(t, nil)
	defer mgr.Stop()
	mgr.stateMap = sm

	p := testPeer("remote-node")
	mgr.sendStateSyncResponseTo(p, nil)

	if capture.Count(slog.LevelWarn) == 0 {
		t.Fatal("expected warning for truncated response")
	}

	select {
	case data := <-p.sendCh:
		if len(data) == 0 {
			t.Fatal("empty envelope sent")
		}
	case <-time.After(time.Second):
		t.Fatal("no response sent despite truncation")
	}
}

// --- stateSyncRequestHandler tests ---

func TestStateSyncRequestHandlerFirstRequest(t *testing.T) {
	mgr := testManager(t, nil)
	defer func() { removePeers(mgr); mgr.Stop() }()

	mgr.stateMap.Set("data", []byte("val")) //nolint:errcheck

	p := testPeer("peer-1")
	mgr.mu.Lock()
	mgr.peers["peer-1"] = p
	mgr.mu.Unlock()

	handler := mgr.stateSyncRequestHandler()
	handler("peer-1", nil)

	// Should send a response (handler spawns goroutine)
	select {
	case data := <-p.sendCh:
		if len(data) == 0 {
			t.Fatal("empty response")
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not send response")
	}
}

func TestStateSyncRequestHandlerRateLimit(t *testing.T) {
	capture := logging.CaptureForTest()
	defer capture.Restore()

	mgr := testManager(t, nil)
	defer func() { removePeers(mgr); mgr.Stop() }()

	mgr.stateMap.Set("data", []byte("val")) //nolint:errcheck

	p := testPeer("peer-1")
	mgr.mu.Lock()
	mgr.peers["peer-1"] = p
	mgr.mu.Unlock()

	handler := mgr.stateSyncRequestHandler()

	// First request: OK
	handler("peer-1", nil)
	time.Sleep(100 * time.Millisecond) // let goroutine send

	// Second request within cooldown: rate-limited
	handler("peer-1", nil)
	time.Sleep(100 * time.Millisecond)

	if capture.Count(slog.LevelWarn) == 0 {
		t.Fatal("expected rate-limit warning")
	}
}

func TestStateSyncRequestHandlerUnknownPeer(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	handler := mgr.stateSyncRequestHandler()
	// No peers registered — should not panic
	handler("unknown-peer", nil)
}

// --- stateSyncResponseHandler tests ---

func TestStateSyncResponseHandlerMergesEntries(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	// Create a signed entry from a remote node
	_, remotePriv, _ := ed25519.GenerateKey(nil)
	remotePub := remotePriv.Public().(ed25519.PublicKey)
	remoteHash := sha256.Sum256(remotePub)
	remoteNodeID := hex.EncodeToString(remoteHash[:])

	remoteMap := state.NewMap(remoteNodeID, remotePriv)
	entry, _, _ := remoteMap.Set("color", []byte("red"))

	handler := mgr.stateSyncResponseHandler()
	handler(remoteNodeID, stateToPBEntries(entry))

	// Verify merged into local map
	got, ok := mgr.stateMap.Get(remoteNodeID + "/color")
	if !ok {
		t.Fatal("entry not merged into local state map")
	}
	if string(got.Value) != "red" {
		t.Fatalf("value = %q, want %q", got.Value, "red")
	}
}

func TestStateSyncResponseHandlerEmptyEntries(t *testing.T) {
	mgr := testManager(t, nil)
	defer mgr.Stop()

	handler := mgr.stateSyncResponseHandler()
	// Should not panic with empty/nil entries
	handler("peer-1", nil)
}

// stateToPBEntries converts state.Entry values to protobuf StateEntry pointers.
func stateToPBEntries(entries ...state.Entry) []*pb.StateEntry {
	result := make([]*pb.StateEntry, len(entries))
	for i, e := range entries {
		result[i] = &pb.StateEntry{
			Key:          e.Key,
			Value:        e.Value,
			LamportTs:    e.LamportTs,
			NodeId:       e.NodeID,
			Deleted:      e.Deleted,
			Signature:    e.Signature,
			AuthorPubkey: e.AuthorPubkey,
		}
	}
	return result
}

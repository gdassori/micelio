package state

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	boltstore "micelio/internal/store/bolt"
)

func tempStore(t *testing.T) *boltstore.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := boltstore.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// persistTestID generates an identity for persistence tests.
func persistTestID(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	hash := sha256.Sum256(pub)
	return hex.EncodeToString(hash[:]), priv
}

// persistTestEntry creates a signed entry for persistence tests.
func persistTestEntry(nodeID string, privKey ed25519.PrivateKey, subkey string, value []byte, ts uint64, deleted bool) Entry {
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

func TestPersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	idA, privA := persistTestID(t)
	idB, privB := persistTestID(t)

	// Create store, persist signed entries
	st, err := boltstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	entries := []Entry{
		persistTestEntry(idA, privA, "color", []byte("blue"), 5, false),
		persistTestEntry(idB, privB, "size", []byte("large"), 10, false),
		persistTestEntry(idA, privA, "shape", []byte("round"), 3, false),
	}
	for _, e := range entries {
		PersistEntry(st, e)
	}
	_ = st.Close()

	// Reopen store, load into a new map
	st2, err := boltstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()

	idC, privC := persistTestID(t)
	m := NewMap(idC, privC)
	if err := m.LoadFromStore(st2); err != nil {
		t.Fatal(err)
	}

	if m.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", m.Len())
	}

	for _, want := range entries {
		got, ok := m.Get(want.Key)
		if !ok {
			t.Fatalf("key %q not found", want.Key)
		}
		if string(got.Value) != string(want.Value) {
			t.Fatalf("key %q: Value = %q, want %q", want.Key, got.Value, want.Value)
		}
		if got.LamportTs != want.LamportTs {
			t.Fatalf("key %q: LamportTs = %d, want %d", want.Key, got.LamportTs, want.LamportTs)
		}
		if got.NodeID != want.NodeID {
			t.Fatalf("key %q: NodeID = %q, want %q", want.Key, got.NodeID, want.NodeID)
		}
	}
}

func TestLoadSetsClockFloor(t *testing.T) {
	st := tempStore(t)
	idA, privA := persistTestID(t)
	idB, privB := persistTestID(t)

	PersistEntry(st, persistTestEntry(idA, privA, "a", []byte("1"), 42, false))
	PersistEntry(st, persistTestEntry(idB, privB, "b", []byte("2"), 99, false))

	idC, privC := persistTestID(t)
	m := NewMap(idC, privC)
	if err := m.LoadFromStore(st); err != nil {
		t.Fatal(err)
	}

	if m.Clock().Current() != 99 {
		t.Fatalf("Clock.Current() = %d, want 99", m.Clock().Current())
	}

	if got := m.Clock().Tick(); got != 100 {
		t.Fatalf("Clock.Tick() = %d, want 100", got)
	}
}

func TestLoadFromStoreEmpty(t *testing.T) {
	st := tempStore(t)
	idA, privA := persistTestID(t)
	m := NewMap(idA, privA)
	if err := m.LoadFromStore(st); err != nil {
		t.Fatal(err)
	}
	if m.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", m.Len())
	}
}

func TestPersistEntryNilStore(t *testing.T) {
	idA, privA := persistTestID(t)
	e := persistTestEntry(idA, privA, "key", []byte("val"), 1, false)
	PersistEntry(nil, e) // should not panic
}

func TestLoadFromStoreNilStore(t *testing.T) {
	idA, privA := persistTestID(t)
	m := NewMap(idA, privA)
	if err := m.LoadFromStore(nil); err != nil {
		t.Fatal(err)
	}
}

func TestPersistEntryCorruptSkipped(t *testing.T) {
	st := tempStore(t)
	idA, privA := persistTestID(t)

	// Write corrupt data directly to the bucket
	if err := st.Set([]byte("state"), []byte("corrupt-key"), []byte("not-a-protobuf")); err != nil {
		t.Fatal(err)
	}
	// Write a valid entry
	e := persistTestEntry(idA, privA, "valid", []byte("ok"), 1, false)
	PersistEntry(st, e)

	idB, privB := persistTestID(t)
	m := NewMap(idB, privB)
	if err := m.LoadFromStore(st); err != nil {
		t.Fatal(err)
	}

	// Only the valid entry should be loaded (corrupt skipped)
	if m.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", m.Len())
	}
	_, ok := m.Get(e.Key)
	if !ok {
		t.Fatal("valid key not found")
	}
}

func TestPersistAndLoadTombstone(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	idA, privA := persistTestID(t)

	// Persist a tombstone and a live entry
	st1, err := boltstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	deadEntry := persistTestEntry(idA, privA, "dead", nil, 5, true)
	liveEntry := persistTestEntry(idA, privA, "live", []byte("val"), 3, false)
	PersistEntry(st1, deadEntry)
	PersistEntry(st1, liveEntry)
	_ = st1.Close()

	// Reload
	st2, err := boltstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()

	idB, privB := persistTestID(t)
	m := NewMap(idB, privB)
	if err := m.LoadFromStore(st2); err != nil {
		t.Fatal(err)
	}

	if m.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", m.Len())
	}

	// Get hides tombstone
	_, found := m.Get(deadEntry.Key)
	if found {
		t.Fatal("tombstone should be hidden by Get")
	}

	// Live entry is visible
	got, found := m.Get(liveEntry.Key)
	if !found || string(got.Value) != "val" {
		t.Fatal("live entry should be visible")
	}
}

func TestPersistAndLoadSignatureRoundTrip(t *testing.T) {
	st := tempStore(t)
	idA, privA := persistTestID(t)

	original := persistTestEntry(idA, privA, "key", []byte("val"), 42, false)
	PersistEntry(st, original)

	idB, privB := persistTestID(t)
	m := NewMap(idB, privB)
	if err := m.LoadFromStore(st); err != nil {
		t.Fatal(err)
	}

	got, ok := m.Get(original.Key)
	if !ok {
		t.Fatal("key not found after reload")
	}

	// Verify signature fields survived persistence round-trip
	if !VerifyEntry(got) {
		t.Fatal("entry signature should still verify after persistence round-trip")
	}
}

func TestDeleteEntryFromStore(t *testing.T) {
	st := tempStore(t)
	idA, privA := persistTestID(t)

	e := persistTestEntry(idA, privA, "color", []byte("blue"), 1, false)
	PersistEntry(st, e)

	// Delete from store
	DeleteEntry(st, e.Key)

	// Load into a new map — should be empty
	idB, privB := persistTestID(t)
	m := NewMap(idB, privB)
	if err := m.LoadFromStore(st); err != nil {
		t.Fatal(err)
	}
	if m.Len() != 0 {
		t.Fatalf("Len() = %d, want 0 after DeleteEntry", m.Len())
	}
}

func TestDeleteEntryNilStore(t *testing.T) {
	DeleteEntry(nil, "key") // should not panic
}

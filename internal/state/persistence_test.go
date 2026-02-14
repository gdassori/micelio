package state

import (
	"os"
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

func TestPersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Create store, persist entries
	st, err := boltstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	entries := []Entry{
		{Key: "color", Value: []byte("blue"), LamportTs: 5, NodeID: "node-a"},
		{Key: "size", Value: []byte("large"), LamportTs: 10, NodeID: "node-b"},
		{Key: "shape", Value: []byte("round"), LamportTs: 3, NodeID: "node-a"},
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

	m := NewMap("node-c")
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

	PersistEntry(st, Entry{Key: "a", Value: []byte("1"), LamportTs: 42, NodeID: "node-a"})
	PersistEntry(st, Entry{Key: "b", Value: []byte("2"), LamportTs: 99, NodeID: "node-b"})

	m := NewMap("node-c")
	if err := m.LoadFromStore(st); err != nil {
		t.Fatal(err)
	}

	if m.Clock().Current() != 99 {
		t.Fatalf("Clock.Current() = %d, want 99", m.Clock().Current())
	}

	// Next Tick should be 100
	if got := m.Clock().Tick(); got != 100 {
		t.Fatalf("Clock.Tick() = %d, want 100", got)
	}
}

func TestLoadFromStoreEmpty(t *testing.T) {
	st := tempStore(t)
	m := NewMap("node-a")
	if err := m.LoadFromStore(st); err != nil {
		t.Fatal(err)
	}
	if m.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", m.Len())
	}
}

func TestPersistEntryNilStore(t *testing.T) {
	// Should not panic
	PersistEntry(nil, Entry{Key: "key", Value: []byte("val"), LamportTs: 1, NodeID: "node-a"})
}

func TestLoadFromStoreNilStore(t *testing.T) {
	m := NewMap("node-a")
	if err := m.LoadFromStore(nil); err != nil {
		t.Fatal(err)
	}
}

func TestPersistEntryCorruptSkipped(t *testing.T) {
	st := tempStore(t)

	// Write corrupt data directly to the bucket
	if err := st.Set([]byte("state"), []byte("corrupt-key"), []byte("not-a-protobuf")); err != nil {
		t.Fatal(err)
	}
	// Write a valid entry
	PersistEntry(st, Entry{Key: "valid", Value: []byte("ok"), LamportTs: 1, NodeID: "node-a"})

	m := NewMap("node-b")
	if err := m.LoadFromStore(st); err != nil {
		t.Fatal(err)
	}

	// Only the valid entry should be loaded
	if m.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", m.Len())
	}
	_, ok := m.Get("valid")
	if !ok {
		t.Fatal("valid key not found")
	}
}

func init() {
	// Suppress "data directory does not exist" warnings in test output
	_ = os.MkdirAll(os.TempDir(), 0700)
}

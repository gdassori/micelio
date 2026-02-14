package state

import (
	"strings"
	"sync"
	"testing"
)

func TestMapSetAndGet(t *testing.T) {
	m := NewMap("node-a")
	entry, ok, err := m.Set("color", []byte("blue"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Set rejected")
	}
	if entry.LamportTs != 1 {
		t.Fatalf("LamportTs = %d, want 1", entry.LamportTs)
	}

	got, found := m.Get("color")
	if !found {
		t.Fatal("Get returned not found")
	}
	if string(got.Value) != "blue" {
		t.Fatalf("Value = %q, want %q", got.Value, "blue")
	}
	if got.NodeID != "node-a" {
		t.Fatalf("NodeID = %q, want %q", got.NodeID, "node-a")
	}
}

func TestMapGetNotFound(t *testing.T) {
	m := NewMap("node-a")
	_, found := m.Get("missing")
	if found {
		t.Fatal("expected not found")
	}
}

func TestMapLWWHigherTsWins(t *testing.T) {
	m := NewMap("node-a")
	m.Set("key", []byte("old"))

	won := m.Merge(Entry{Key: "key", Value: []byte("new"), LamportTs: 100, NodeID: "node-b"})
	if !won {
		t.Fatal("higher ts should win")
	}
	got, _ := m.Get("key")
	if string(got.Value) != "new" {
		t.Fatalf("Value = %q, want %q", got.Value, "new")
	}
}

func TestMapLWWLowerTsLoses(t *testing.T) {
	m := NewMap("node-a")

	// Set a value with high ts via merge
	m.Merge(Entry{Key: "key", Value: []byte("winner"), LamportTs: 100, NodeID: "node-b"})

	// Try to merge a lower ts — should be rejected
	won := m.Merge(Entry{Key: "key", Value: []byte("loser"), LamportTs: 50, NodeID: "node-c"})
	if won {
		t.Fatal("lower ts should lose")
	}
	got, _ := m.Get("key")
	if string(got.Value) != "winner" {
		t.Fatalf("Value = %q, want %q", got.Value, "winner")
	}
}

func TestMapLWWTieBreakerNodeID(t *testing.T) {
	m := NewMap("node-a")

	m.Merge(Entry{Key: "key", Value: []byte("from-b"), LamportTs: 5, NodeID: "node-b"})
	won := m.Merge(Entry{Key: "key", Value: []byte("from-c"), LamportTs: 5, NodeID: "node-c"})
	if !won {
		t.Fatal("higher node_id should win tie")
	}
	got, _ := m.Get("key")
	if string(got.Value) != "from-c" {
		t.Fatalf("Value = %q, want %q", got.Value, "from-c")
	}

	// node-a has lower node_id, should lose tie
	won = m.Merge(Entry{Key: "key", Value: []byte("from-a"), LamportTs: 5, NodeID: "node-a"})
	if won {
		t.Fatal("lower node_id should lose tie")
	}
	got, _ = m.Get("key")
	if string(got.Value) != "from-c" {
		t.Fatalf("Value = %q, want %q", got.Value, "from-c")
	}
}

func TestMapSnapshot(t *testing.T) {
	m := NewMap("node-a")
	m.Set("a", []byte("1"))
	m.Set("b", []byte("2"))
	m.Set("c", []byte("3"))

	snap := m.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}

	keys := make(map[string]bool)
	for _, e := range snap {
		keys[e.Key] = true
	}
	for _, k := range []string{"a", "b", "c"} {
		if !keys[k] {
			t.Fatalf("missing key %q in snapshot", k)
		}
	}
}

func TestMapLen(t *testing.T) {
	m := NewMap("node-a")
	if m.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", m.Len())
	}
	m.Set("x", []byte("1"))
	if m.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", m.Len())
	}
}

func TestMapChangeHandlerCalledOnSet(t *testing.T) {
	m := NewMap("node-a")
	var called Entry
	m.SetChangeHandler(func(entry Entry) {
		called = entry
	})

	m.Set("color", []byte("red"))
	if called.Key != "color" || string(called.Value) != "red" {
		t.Fatalf("ChangeHandler not called correctly: got %+v", called)
	}
}

func TestMapChangeHandlerNotCalledOnMerge(t *testing.T) {
	m := NewMap("node-a")
	callCount := 0
	m.SetChangeHandler(func(entry Entry) {
		callCount++
	})

	m.Merge(Entry{Key: "key", Value: []byte("val"), LamportTs: 10, NodeID: "node-b"})
	if callCount != 0 {
		t.Fatalf("ChangeHandler called %d times on Merge, want 0", callCount)
	}
}

func TestMapConcurrentAccess(t *testing.T) {
	m := NewMap("node-a")
	var wg sync.WaitGroup
	n := 100

	wg.Add(n * 3)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			m.Set("key", []byte("val"))
		}()
		go func(ts uint64) {
			defer wg.Done()
			m.Merge(Entry{Key: "key", Value: []byte("remote"), LamportTs: ts, NodeID: "node-b"})
		}(uint64(i))
		go func() {
			defer wg.Done()
			m.Get("key")
		}()
	}
	wg.Wait()

	// Should have exactly one key
	if m.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", m.Len())
	}
}

func TestEntryWins(t *testing.T) {
	tests := []struct {
		name string
		a, b Entry
		want bool
	}{
		{"higher ts wins", Entry{LamportTs: 10, NodeID: "a"}, Entry{LamportTs: 5, NodeID: "z"}, true},
		{"lower ts loses", Entry{LamportTs: 5, NodeID: "z"}, Entry{LamportTs: 10, NodeID: "a"}, false},
		{"tie: higher node wins", Entry{LamportTs: 5, NodeID: "z"}, Entry{LamportTs: 5, NodeID: "a"}, true},
		{"tie: lower node loses", Entry{LamportTs: 5, NodeID: "a"}, Entry{LamportTs: 5, NodeID: "z"}, false},
		{"tie: same node", Entry{LamportTs: 5, NodeID: "a"}, Entry{LamportTs: 5, NodeID: "a"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Wins(tt.b); got != tt.want {
				t.Errorf("(%v).Wins(%v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// --- Security validation tests ---

func TestMapRejectsOversizedKey(t *testing.T) {
	m := NewMap("node-a")
	longKey := strings.Repeat("x", MaxKeyLen+1)
	_, _, err := m.Set(longKey, []byte("val"))
	if err != ErrKeyTooLong {
		t.Fatalf("expected ErrKeyTooLong, got %v", err)
	}
}

func TestMapRejectsOversizedValue(t *testing.T) {
	m := NewMap("node-a")
	longVal := make([]byte, MaxValueLen+1)
	_, _, err := m.Set("key", longVal)
	if err != ErrValueTooLong {
		t.Fatalf("expected ErrValueTooLong, got %v", err)
	}
}

func TestMapMergeRejectsOversized(t *testing.T) {
	m := NewMap("node-a")

	won := m.Merge(Entry{Key: strings.Repeat("x", MaxKeyLen+1), Value: []byte("v"), LamportTs: 1, NodeID: "b"})
	if won {
		t.Fatal("oversized key should be rejected")
	}

	won = m.Merge(Entry{Key: "k", Value: make([]byte, MaxValueLen+1), LamportTs: 1, NodeID: "b"})
	if won {
		t.Fatal("oversized value should be rejected")
	}
}

func TestMapMergeRejectsUnsafeTimestamp(t *testing.T) {
	m := NewMap("node-a")

	won := m.Merge(Entry{Key: "key", Value: []byte("val"), LamportTs: MaxSafeLamportTs + 1, NodeID: "b"})
	if won {
		t.Fatal("unsafe timestamp should be rejected")
	}

	// Clock should NOT have been advanced
	if m.Clock().Current() != 0 {
		t.Fatalf("clock should still be 0, got %d", m.Clock().Current())
	}
}

func TestMapRejectsNewKeysAtCapacity(t *testing.T) {
	m := NewMap("node-a")

	// Fill to capacity
	for i := 0; i < MaxStateEntries; i++ {
		m.Merge(Entry{Key: string(rune(i)), Value: []byte("v"), LamportTs: uint64(i + 1), NodeID: "b"})
	}
	if m.Len() != MaxStateEntries {
		t.Fatalf("Len() = %d, want %d", m.Len(), MaxStateEntries)
	}

	// New key should be rejected
	won := m.Merge(Entry{Key: "new-key", Value: []byte("v"), LamportTs: 999999, NodeID: "b"})
	if won {
		t.Fatal("new key at capacity should be rejected")
	}

	// Update to existing key should still work
	won = m.Merge(Entry{Key: string(rune(0)), Value: []byte("updated"), LamportTs: 999999, NodeID: "b"})
	if !won {
		t.Fatal("update to existing key should still work at capacity")
	}
}

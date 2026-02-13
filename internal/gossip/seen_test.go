package gossip

import (
	"testing"
	"time"
)

func TestSeenCacheBasic(t *testing.T) {
	sc := NewSeenCache(5 * time.Minute)

	// First check: not seen
	if sc.Check("msg-1") {
		t.Fatal("expected msg-1 to not be seen")
	}

	// Second check: seen
	if !sc.Check("msg-1") {
		t.Fatal("expected msg-1 to be seen")
	}

	// Different ID: not seen
	if sc.Check("msg-2") {
		t.Fatal("expected msg-2 to not be seen")
	}
}

func TestSeenCacheLen(t *testing.T) {
	sc := NewSeenCache(5 * time.Minute)
	sc.Check("a")
	sc.Check("b")
	sc.Check("c")
	if sc.Len() != 3 {
		t.Fatalf("expected 3, got %d", sc.Len())
	}
}

func TestSeenCacheCleanup(t *testing.T) {
	sc := NewSeenCache(50 * time.Millisecond)

	sc.Check("msg-1")
	if sc.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", sc.Len())
	}

	// Wait for TTL to expire
	time.Sleep(100 * time.Millisecond)
	sc.cleanup()

	if sc.Len() != 0 {
		t.Fatalf("expected 0 entries after cleanup, got %d", sc.Len())
	}

	// msg-1 should be "new" again
	if sc.Check("msg-1") {
		t.Fatal("expected msg-1 to not be seen after cleanup")
	}
}

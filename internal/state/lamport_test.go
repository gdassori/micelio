package state

import (
	"sync"
	"testing"
)

func TestLamportTick(t *testing.T) {
	var c LamportClock
	for i := uint64(1); i <= 10; i++ {
		if got := c.Tick(); got != i {
			t.Fatalf("Tick() = %d, want %d", got, i)
		}
	}
}

func TestLamportWitnessHigher(t *testing.T) {
	var c LamportClock
	c.Tick() // counter = 1

	got := c.Witness(100)
	if got != 101 {
		t.Fatalf("Witness(100) = %d, want 101", got)
	}
	if c.Current() != 101 {
		t.Fatalf("Current() = %d, want 101", c.Current())
	}
}

func TestLamportWitnessLower(t *testing.T) {
	var c LamportClock
	for i := 0; i < 10; i++ {
		c.Tick() // counter = 10
	}

	got := c.Witness(5) // observed < current, still ticks
	if got != 11 {
		t.Fatalf("Witness(5) = %d, want 11", got)
	}
}

func TestLamportSetFloor(t *testing.T) {
	var c LamportClock
	c.SetFloor(50)
	if c.Current() != 50 {
		t.Fatalf("Current() = %d, want 50", c.Current())
	}

	// SetFloor with lower value is a no-op
	c.SetFloor(10)
	if c.Current() != 50 {
		t.Fatalf("Current() = %d, want 50 after lower SetFloor", c.Current())
	}
}

func TestLamportConcurrent(t *testing.T) {
	var c LamportClock
	var wg sync.WaitGroup
	n := 100

	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			c.Tick()
		}()
		go func(v uint64) {
			defer wg.Done()
			c.Witness(v)
		}(uint64(i))
	}
	wg.Wait()

	// After n Ticks and n Witnesses, counter must be at least n
	// (each Tick adds 1, each Witness adds at least 1).
	if c.Current() < uint64(n) {
		t.Fatalf("Current() = %d, want >= %d", c.Current(), n)
	}
}

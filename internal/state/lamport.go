package state

import "sync/atomic"

// LamportClock is a monotonically increasing logical clock.
// It is safe for concurrent use.
type LamportClock struct {
	counter atomic.Uint64
}

// Tick increments the clock and returns the new value.
// Used when generating a new local event (e.g., /set command).
func (c *LamportClock) Tick() uint64 {
	return c.counter.Add(1)
}

// Witness updates the clock to be at least the observed value,
// then ticks. Returns the new value.
// Used when receiving a remote state entry.
func (c *LamportClock) Witness(observed uint64) uint64 {
	for {
		current := c.counter.Load()
		next := current
		if observed > next {
			next = observed
		}
		next++
		if c.counter.CompareAndSwap(current, next) {
			return next
		}
	}
}

// Current returns the current clock value without incrementing.
func (c *LamportClock) Current() uint64 {
	return c.counter.Load()
}

// SetFloor ensures the clock is at least floor. Does NOT tick.
// Used during startup to restore from persisted state.
func (c *LamportClock) SetFloor(floor uint64) {
	for {
		current := c.counter.Load()
		if current >= floor {
			return
		}
		if c.counter.CompareAndSwap(current, floor) {
			return
		}
	}
}

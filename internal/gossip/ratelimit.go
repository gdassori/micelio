package gossip

import (
	"sync"
	"time"
)

type senderState struct {
	tokens    float64
	lastCheck time.Time
}

// RateLimiter enforces per-sender message rate limits using a token bucket.
type RateLimiter struct {
	mu        sync.Mutex
	senders   map[string]*senderState
	maxPerSec float64
	burst     float64 // max tokens (2× maxPerSec)
}

// NewRateLimiter creates a rate limiter allowing maxPerSec messages per sender.
func NewRateLimiter(maxPerSec float64) *RateLimiter {
	return &RateLimiter{
		senders:   make(map[string]*senderState),
		maxPerSec: maxPerSec,
		burst:     maxPerSec * 2,
	}
}

// Allow returns true if the sender is within the rate limit.
// Each call consumes one token.
func (r *RateLimiter) Allow(senderID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	state, ok := r.senders[senderID]
	if !ok {
		r.senders[senderID] = &senderState{
			tokens:    r.burst - 1,
			lastCheck: now,
		}
		return true
	}

	// Refill tokens based on elapsed time
	elapsed := now.Sub(state.lastCheck).Seconds()
	state.tokens += elapsed * r.maxPerSec
	if state.tokens > r.burst {
		state.tokens = r.burst
	}
	state.lastCheck = now

	if state.tokens >= 1 {
		state.tokens--
		return true
	}
	return false
}

// CleanupLoop periodically removes stale sender entries until done is closed.
func (r *RateLimiter) CleanupLoop(done <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.cleanup()
		case <-done:
			return
		}
	}
}

func (r *RateLimiter) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	for id, state := range r.senders {
		if state.lastCheck.Before(cutoff) {
			delete(r.senders, id)
		}
	}
}

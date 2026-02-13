package gossip

import (
	"testing"
	"time"
)

func TestRateLimiterAllow(t *testing.T) {
	rl := NewRateLimiter(5.0) // 5 per second, burst=10

	// First 10 calls should all be allowed (burst)
	for i := 0; i < 10; i++ {
		if !rl.Allow("sender-a") {
			t.Fatalf("call %d should be allowed", i)
		}
	}

	// 11th call should be rejected (burst exhausted)
	if rl.Allow("sender-a") {
		t.Fatal("expected rate limit to reject")
	}
}

func TestRateLimiterRefill(t *testing.T) {
	rl := NewRateLimiter(100.0) // 100 per second, burst=200

	// Exhaust burst
	for i := 0; i < 200; i++ {
		rl.Allow("sender-a")
	}
	if rl.Allow("sender-a") {
		t.Fatal("expected exhausted")
	}

	// Wait for refill
	time.Sleep(50 * time.Millisecond) // ~5 tokens at 100/s
	if !rl.Allow("sender-a") {
		t.Fatal("expected allowed after refill")
	}
}

func TestRateLimiterIndependentSenders(t *testing.T) {
	rl := NewRateLimiter(5.0) // burst=10

	// Exhaust sender-a
	for i := 0; i < 10; i++ {
		rl.Allow("sender-a")
	}
	if rl.Allow("sender-a") {
		t.Fatal("sender-a should be exhausted")
	}

	// sender-b should still be allowed
	if !rl.Allow("sender-b") {
		t.Fatal("sender-b should be allowed")
	}
}

func TestRateLimiterCleanup(t *testing.T) {
	rl := NewRateLimiter(5.0)
	rl.Allow("sender-a")

	// Force the lastCheck to be old for cleanup
	rl.mu.Lock()
	rl.senders["sender-a"].lastCheck = time.Now().Add(-10 * time.Minute)
	rl.mu.Unlock()

	rl.cleanup()

	rl.mu.Lock()
	_, exists := rl.senders["sender-a"]
	rl.mu.Unlock()
	if exists {
		t.Fatal("expected sender-a to be cleaned up")
	}
}

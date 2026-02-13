package gossip

import (
	"sync"
	"time"
)

// SeenCache tracks recently seen message IDs for deduplication.
// Thread-safe. Entries expire after a configurable TTL.
type SeenCache struct {
	mu      sync.RWMutex
	entries map[string]time.Time
	ttl     time.Duration
}

// NewSeenCache creates a seen cache with the given TTL for entries.
func NewSeenCache(ttl time.Duration) *SeenCache {
	return &SeenCache{
		entries: make(map[string]time.Time),
		ttl:     ttl,
	}
}

// Has returns true if the messageID was already seen (read-only).
func (s *SeenCache) Has(messageID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.entries[messageID]
	return ok
}

// Check returns true if the messageID was already seen.
// If not seen, it marks the messageID as seen and returns false.
// This is an atomic test-and-set operation.
func (s *SeenCache) Check(messageID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[messageID]; ok {
		return true
	}
	s.entries[messageID] = time.Now()
	return false
}

// Len returns the number of entries (for testing).
func (s *SeenCache) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// CleanupLoop periodically removes expired entries until done is closed.
func (s *SeenCache) CleanupLoop(done <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.cleanup()
		case <-done:
			return
		}
	}
}

func (s *SeenCache) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-s.ttl)
	for id, t := range s.entries {
		if t.Before(cutoff) {
			delete(s.entries, id)
		}
	}
}

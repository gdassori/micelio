package gossip

import (
	"crypto/ed25519"
	"sync"
)

// TrustLevel indicates how a peer's public key was learned.
type TrustLevel int

const (
	// TrustDirectlyVerified means the key was learned via a direct Noise
	// handshake — the strongest level of trust.
	TrustDirectlyVerified TrustLevel = iota
	// TrustGossipLearned means the key was learned via gossip, such as
	// peer exchange or auto-learning from the sender_pubkey envelope field.
	// Not independently verified, but usable for signature checks.
	TrustGossipLearned
)

// KeyEntry holds a peer's public key and how it was learned.
type KeyEntry struct {
	PubKey     ed25519.PublicKey
	TrustLevel TrustLevel
}

// KeyRing is a thread-safe map of nodeID → KeyEntry.
type KeyRing struct {
	mu   sync.RWMutex
	keys map[string]KeyEntry
}

// NewKeyRing creates an empty keyring.
func NewKeyRing() *KeyRing {
	return &KeyRing{
		keys: make(map[string]KeyEntry),
	}
}

// Add inserts or upgrades a key in the keyring.
// A directly-verified key is never downgraded by a gossip-learned key.
// A gossip-learned key can be upgraded to directly-verified.
func (kr *KeyRing) Add(nodeID string, pubKey ed25519.PublicKey, trust TrustLevel) {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	existing, ok := kr.keys[nodeID]
	if ok && trust >= existing.TrustLevel {
		// Don't downgrade: existing is at same or stronger trust.
		return
	}
	copiedKey := make(ed25519.PublicKey, len(pubKey))
	copy(copiedKey, pubKey)
	kr.keys[nodeID] = KeyEntry{PubKey: copiedKey, TrustLevel: trust}
}

// Lookup returns the key entry for a node, or false if unknown.
func (kr *KeyRing) Lookup(nodeID string) (KeyEntry, bool) {
	kr.mu.RLock()
	defer kr.mu.RUnlock()
	entry, ok := kr.keys[nodeID]
	return entry, ok
}

// Remove deletes a node from the keyring.
func (kr *KeyRing) Remove(nodeID string) {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	delete(kr.keys, nodeID)
}

// Len returns the number of entries in the keyring.
func (kr *KeyRing) Len() int {
	kr.mu.RLock()
	defer kr.mu.RUnlock()
	return len(kr.keys)
}

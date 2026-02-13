package gossip

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func generateKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub
}

func TestKeyRingAddAndLookup(t *testing.T) {
	kr := NewKeyRing()
	pub := generateKey(t)

	kr.Add("node-a", pub, TrustDirectlyVerified)

	entry, ok := kr.Lookup("node-a")
	if !ok {
		t.Fatal("expected to find node-a")
	}
	if entry.TrustLevel != TrustDirectlyVerified {
		t.Fatalf("expected TrustDirectlyVerified, got %d", entry.TrustLevel)
	}
	if !entry.PubKey.Equal(pub) {
		t.Fatal("pubkey mismatch")
	}
}

func TestKeyRingUnknownNode(t *testing.T) {
	kr := NewKeyRing()
	_, ok := kr.Lookup("unknown")
	if ok {
		t.Fatal("expected not found for unknown node")
	}
}

func TestKeyRingNoDowngrade(t *testing.T) {
	kr := NewKeyRing()
	pub := generateKey(t)
	pub2 := generateKey(t)

	kr.Add("node-a", pub, TrustDirectlyVerified)
	// Try to downgrade to gossip-learned with a different key
	kr.Add("node-a", pub2, TrustGossipLearned)

	entry, _ := kr.Lookup("node-a")
	if entry.TrustLevel != TrustDirectlyVerified {
		t.Fatal("trust level was downgraded")
	}
	if !entry.PubKey.Equal(pub) {
		t.Fatal("pubkey was changed by weaker trust")
	}
}

func TestKeyRingUpgrade(t *testing.T) {
	kr := NewKeyRing()
	pub := generateKey(t)
	pub2 := generateKey(t)

	kr.Add("node-a", pub, TrustGossipLearned)
	kr.Add("node-a", pub2, TrustDirectlyVerified)

	entry, _ := kr.Lookup("node-a")
	if entry.TrustLevel != TrustDirectlyVerified {
		t.Fatal("trust level was not upgraded")
	}
	if !entry.PubKey.Equal(pub2) {
		t.Fatal("pubkey was not updated on upgrade")
	}
}

func TestKeyRingRemove(t *testing.T) {
	kr := NewKeyRing()
	kr.Add("node-a", generateKey(t), TrustDirectlyVerified)

	kr.Remove("node-a")

	_, ok := kr.Lookup("node-a")
	if ok {
		t.Fatal("expected node-a to be removed")
	}
}

func TestKeyRingLen(t *testing.T) {
	kr := NewKeyRing()
	if kr.Len() != 0 {
		t.Fatalf("expected 0, got %d", kr.Len())
	}
	kr.Add("a", generateKey(t), TrustDirectlyVerified)
	kr.Add("b", generateKey(t), TrustGossipLearned)
	if kr.Len() != 2 {
		t.Fatalf("expected 2, got %d", kr.Len())
	}
}

func TestKeyRingSameLevel(t *testing.T) {
	kr := NewKeyRing()
	pub := generateKey(t)
	pub2 := generateKey(t)

	kr.Add("node-a", pub, TrustGossipLearned)
	// Same trust level — should NOT overwrite (>= check)
	kr.Add("node-a", pub2, TrustGossipLearned)

	entry, _ := kr.Lookup("node-a")
	if !entry.PubKey.Equal(pub) {
		t.Fatal("pubkey was overwritten at same trust level")
	}
}

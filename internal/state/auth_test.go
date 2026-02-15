package state

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func makeTestEntry(t *testing.T) (Entry, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	hash := sha256.Sum256(pub)
	nodeID := hex.EncodeToString(hash[:])

	e := Entry{
		Key:       nodeID + "/name",
		Value:     []byte("my-node"),
		LamportTs: 42,
		NodeID:    nodeID,
	}
	SignEntry(&e, priv)
	return e, priv
}

func TestSignAndVerifyEntry(t *testing.T) {
	e, _ := makeTestEntry(t)

	if !VerifyEntry(e) {
		t.Fatal("valid signed entry should verify")
	}
	if len(e.Signature) != ed25519.SignatureSize {
		t.Fatalf("Signature len = %d, want %d", len(e.Signature), ed25519.SignatureSize)
	}
	if len(e.AuthorPubkey) != ed25519.PublicKeySize {
		t.Fatalf("AuthorPubkey len = %d, want %d", len(e.AuthorPubkey), ed25519.PublicKeySize)
	}
}

func TestVerifyEntryBadSignature(t *testing.T) {
	e, _ := makeTestEntry(t)

	// Corrupt signature
	e.Signature[0] ^= 0xff
	if VerifyEntry(e) {
		t.Fatal("corrupted signature should not verify")
	}
}

func TestVerifyEntryWrongNamespace(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	pub := priv.Public().(ed25519.PublicKey)
	hash := sha256.Sum256(pub)
	nodeID := hex.EncodeToString(hash[:])

	e := Entry{
		Key:       "wrong-prefix/name",
		Value:     []byte("val"),
		LamportTs: 1,
		NodeID:    nodeID,
	}
	SignEntry(&e, priv)

	if VerifyEntry(e) {
		t.Fatal("entry with wrong namespace should not verify")
	}
}

func TestVerifyEntryPubkeyMismatch(t *testing.T) {
	e, _ := makeTestEntry(t)

	// Replace pubkey with a different one (nodeID won't match)
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	otherPub := otherPriv.Public().(ed25519.PublicKey)
	e.AuthorPubkey = otherPub

	if VerifyEntry(e) {
		t.Fatal("entry with mismatched pubkey should not verify")
	}
}

func TestVerifyEntryMissingPubkey(t *testing.T) {
	e, _ := makeTestEntry(t)
	e.AuthorPubkey = nil

	if VerifyEntry(e) {
		t.Fatal("entry with nil pubkey should not verify")
	}
}

func TestVerifyEntryTamperedValue(t *testing.T) {
	e, _ := makeTestEntry(t)
	e.Value = []byte("tampered")

	if VerifyEntry(e) {
		t.Fatal("entry with tampered value should not verify")
	}
}

func TestVerifyEntryTamperedTimestamp(t *testing.T) {
	e, _ := makeTestEntry(t)
	e.LamportTs = 999

	if VerifyEntry(e) {
		t.Fatal("entry with tampered timestamp should not verify")
	}
}

func TestVerifyEntryTamperedDeleted(t *testing.T) {
	e, _ := makeTestEntry(t)
	e.Deleted = true

	if VerifyEntry(e) {
		t.Fatal("entry with tampered deleted flag should not verify")
	}
}

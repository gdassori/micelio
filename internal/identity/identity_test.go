package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGeneratesNewIdentity(t *testing.T) {
	dir := t.TempDir()
	id, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(id.PrivateKey) != ed25519.PrivateKeySize {
		t.Errorf("private key length: got %d, want %d", len(id.PrivateKey), ed25519.PrivateKeySize)
	}
	if len(id.PublicKey) != ed25519.PublicKeySize {
		t.Errorf("public key length: got %d, want %d", len(id.PublicKey), ed25519.PublicKeySize)
	}

	// NodeID should be hex(sha256(pubkey))
	hash := sha256.Sum256(id.PublicKey)
	want := hex.EncodeToString(hash[:])
	if id.NodeID != want {
		t.Errorf("NodeID: got %q, want %q", id.NodeID, want)
	}
	if len(id.NodeID) != 64 {
		t.Errorf("NodeID length: got %d, want 64", len(id.NodeID))
	}

	if id.SSHSigner == nil {
		t.Error("SSHSigner should not be nil")
	}

	// Key files should exist on disk
	if _, err := os.Stat(filepath.Join(dir, "identity", "node.key")); err != nil {
		t.Errorf("private key file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity", "node.pub")); err != nil {
		t.Errorf("public key file missing: %v", err)
	}
}

func TestLoadReadsExistingIdentity(t *testing.T) {
	dir := t.TempDir()

	// Generate
	id1, err := Load(dir)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}

	// Reload
	id2, err := Load(dir)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}

	if id1.NodeID != id2.NodeID {
		t.Errorf("NodeID mismatch: %q vs %q", id1.NodeID, id2.NodeID)
	}
	if !id1.PublicKey.Equal(id2.PublicKey) {
		t.Error("public keys should match")
	}
}

func TestLoadBadKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "identity")
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Write garbage to key file
	if err := os.WriteFile(filepath.Join(keyDir, "node.key"), []byte("not-a-key"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for bad key file")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	msg := []byte("hello micelio")
	sig := ed25519.Sign(id.PrivateKey, msg)
	if !ed25519.Verify(id.PublicKey, msg, sig) {
		t.Error("signature verification failed")
	}
}

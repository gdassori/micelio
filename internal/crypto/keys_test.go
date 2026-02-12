package crypto_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/curve25519"

	mcrypto "micelio/internal/crypto"
	"micelio/internal/identity"
)

func TestEdToX25519RoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	x25519Priv := mcrypto.EdPrivateToX25519(priv)
	x25519Pub, err := mcrypto.EdPublicToX25519(pub)
	if err != nil {
		t.Fatal(err)
	}

	// Derive public from private via scalar base mult and compare
	derivedPub, err := curve25519.X25519(x25519Priv, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}

	if len(x25519Pub) != 32 {
		t.Fatalf("x25519 public key length: got %d, want 32", len(x25519Pub))
	}

	// The Edwards-to-Montgomery conversion and scalar-base-mult should
	// produce the same public key (they represent the same point on the curve).
	for i := range derivedPub {
		if derivedPub[i] != x25519Pub[i] {
			t.Fatalf("x25519 public key mismatch at byte %d: derived=%x converted=%x", i, derivedPub, x25519Pub)
		}
	}
}

func TestEdPublicToX25519InvalidKey(t *testing.T) {
	// Wrong length should fail the edwards point decoding (expects exactly 32 bytes).
	_, err := mcrypto.EdPublicToX25519([]byte("short"))
	if err == nil {
		t.Fatal("expected error for invalid ed25519 public key")
	}
}

func TestEdToNoiseKeypair(t *testing.T) {
	dir := t.TempDir()
	id, err := identity.Load(dir)
	if err != nil {
		t.Fatalf("Load identity: %v", err)
	}

	kp, err := mcrypto.EdToNoiseKeypair(id)
	if err != nil {
		t.Fatalf("EdToNoiseKeypair: %v", err)
	}

	if len(kp.Private) != 32 {
		t.Errorf("noise private key length: got %d, want 32", len(kp.Private))
	}
	if len(kp.Public) != 32 {
		t.Errorf("noise public key length: got %d, want 32", len(kp.Public))
	}

	// Public key should match scalar base mult of private key.
	derivedPub, err := curve25519.X25519(kp.Private, curve25519.Basepoint)
	if err != nil {
		t.Fatalf("X25519 scalar base mult: %v", err)
	}
	for i := range derivedPub {
		if derivedPub[i] != kp.Public[i] {
			t.Fatalf("noise keypair mismatch at byte %d", i)
		}
	}
}

func TestEdToNoiseKeypairDeterministic(t *testing.T) {
	dir := t.TempDir()
	id, err := identity.Load(dir)
	if err != nil {
		t.Fatalf("Load identity: %v", err)
	}

	kp1, err := mcrypto.EdToNoiseKeypair(id)
	if err != nil {
		t.Fatal(err)
	}
	kp2, err := mcrypto.EdToNoiseKeypair(id)
	if err != nil {
		t.Fatal(err)
	}

	for i := range kp1.Private {
		if kp1.Private[i] != kp2.Private[i] {
			t.Fatal("noise private key not deterministic")
		}
	}
	for i := range kp1.Public {
		if kp1.Public[i] != kp2.Public[i] {
			t.Fatal("noise public key not deterministic")
		}
	}
}

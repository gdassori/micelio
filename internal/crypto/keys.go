package crypto

import (
	"crypto/ed25519"
	"crypto/sha512"
	"fmt"

	"filippo.io/edwards25519"
	"github.com/flynn/noise"

	"micelio/internal/identity"
)

// EdPrivateToX25519 derives an X25519 private key from an ED25519 private key.
// This is SHA-512(seed)[:32]; X25519 applies clamping internally.
func EdPrivateToX25519(edPriv ed25519.PrivateKey) []byte {
	h := sha512.Sum512(edPriv.Seed())
	out := make([]byte, 32)
	copy(out, h[:32])
	return out
}

// EdPublicToX25519 converts an ED25519 public key to its X25519 (Montgomery form) equivalent.
func EdPublicToX25519(edPub ed25519.PublicKey) ([]byte, error) {
	p, err := new(edwards25519.Point).SetBytes(edPub)
	if err != nil {
		return nil, fmt.Errorf("invalid ed25519 public key: %w", err)
	}
	return p.BytesMontgomery(), nil
}

// EdToNoiseKeypair returns a noise.DHKey derived from an Identity's ED25519 keypair,
// suitable for use as a Noise XX static key.
func EdToNoiseKeypair(id *identity.Identity) (noise.DHKey, error) {
	priv := EdPrivateToX25519(id.PrivateKey)
	pub, err := EdPublicToX25519(id.PublicKey)
	if err != nil {
		return noise.DHKey{}, err
	}
	return noise.DHKey{Private: priv, Public: pub}, nil
}

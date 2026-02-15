package state

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// SignEntry signs an entry with the given private key and embeds the public key.
// The signature covers all semantic fields: key, value, lamport_ts, node_id, deleted.
func SignEntry(e *Entry, privKey ed25519.PrivateKey) {
	e.AuthorPubkey = privKey.Public().(ed25519.PublicKey)
	e.Signature = ed25519.Sign(privKey, entrySignData(*e))
}

// VerifyEntry checks that an entry is authentic and authorized:
//  1. AuthorPubkey is a valid 32-byte ED25519 public key
//  2. sha256(AuthorPubkey) == NodeID (pubkey matches claimed identity)
//  3. Key starts with NodeID + "/" (author owns the namespace)
//  4. ED25519 signature is valid over the semantic fields
//
// Returns true if all checks pass. Callers should reject entries that fail.
func VerifyEntry(e Entry) bool {
	if len(e.AuthorPubkey) != ed25519.PublicKeySize {
		return false
	}

	hash := sha256.Sum256(e.AuthorPubkey)
	if hex.EncodeToString(hash[:]) != e.NodeID {
		return false
	}

	if !strings.HasPrefix(e.Key, e.NodeID+"/") {
		return false
	}

	return ed25519.Verify(e.AuthorPubkey, entrySignData(e), e.Signature)
}

// entrySignData returns the canonical byte representation for signing/verification.
// Format: key (utf8) || value (raw) || lamport_ts (uint64 BE) || node_id (utf8) || deleted (1 byte)
func entrySignData(e Entry) []byte {
	var buf []byte
	buf = append(buf, []byte(e.Key)...)
	buf = append(buf, e.Value...)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], e.LamportTs)
	buf = append(buf, ts[:]...)
	buf = append(buf, []byte(e.NodeID)...)
	if e.Deleted {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	return buf
}

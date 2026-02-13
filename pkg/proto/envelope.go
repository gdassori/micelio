package proto

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
)

// SignEnvelope signs an envelope with the given ED25519 private key.
// The signed data is: message_id + sender_id + lamport_ts + msg_type + payload.
// hop_count and sender_pubkey are excluded so that intermediate nodes can decrement
// hop_count and include the public key for auto-learn without invalidating the
// originator's signature.
func SignEnvelope(env *Envelope, privKey ed25519.PrivateKey) {
	if len(privKey) != ed25519.PrivateKeySize {
		env.Signature = nil
		return
	}
	env.Signature = ed25519.Sign(privKey, EnvelopeSignData(env))
}

// VerifyEnvelope verifies the ED25519 signature on an envelope.
func VerifyEnvelope(env *Envelope, pubKey ed25519.PublicKey) bool {
	if len(pubKey) != ed25519.PublicKeySize {
		return false
	}
	if len(env.Signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pubKey, EnvelopeSignData(env), env.Signature)
}

// EnvelopeSignData returns the bytes that are signed/verified for an envelope.
func EnvelopeSignData(env *Envelope) []byte {
	var buf bytes.Buffer
	buf.WriteString(env.MessageId)
	buf.WriteString(env.SenderId)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], env.LamportTs)
	buf.Write(ts[:])
	var mt [4]byte
	binary.BigEndian.PutUint32(mt[:], env.MsgType)
	buf.Write(mt[:])
	buf.Write(env.Payload)
	return buf.Bytes()
}

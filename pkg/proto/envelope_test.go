package proto

import (
	"crypto/ed25519"
	"testing"
)

func makeTestEnvelope() *Envelope {
	return &Envelope{
		MessageId: "test-msg-001",
		SenderId:  "test-sender",
		LamportTs: 42,
		MsgType:   3,
		HopCount:  5,
		Payload:   []byte("hello world"),
	}
}

func TestSignAndVerifyEnvelope(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	env := makeTestEnvelope()
	SignEnvelope(env, priv)

	if len(env.Signature) != ed25519.SignatureSize {
		t.Fatalf("expected signature length %d, got %d", ed25519.SignatureSize, len(env.Signature))
	}

	if !VerifyEnvelope(env, pub) {
		t.Fatal("valid signature should verify")
	}
}

func TestSignEnvelope_InvalidKeyLength(t *testing.T) {
	env := makeTestEnvelope()
	SignEnvelope(env, []byte("short-key"))

	if env.Signature != nil {
		t.Fatal("signing with invalid key should leave signature nil")
	}
}

func TestVerifyEnvelope_InvalidPubKeyLength(t *testing.T) {
	env := makeTestEnvelope()
	env.Signature = make([]byte, ed25519.SignatureSize)
	if VerifyEnvelope(env, []byte("short")) {
		t.Fatal("verify with invalid pubkey length should return false")
	}
}

func TestVerifyEnvelope_InvalidSignatureLength(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	env := makeTestEnvelope()
	env.Signature = []byte("bad-sig")
	if VerifyEnvelope(env, pub) {
		t.Fatal("verify with invalid signature length should return false")
	}
}

func TestVerifyEnvelope_WrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)

	env := makeTestEnvelope()
	SignEnvelope(env, priv)

	if VerifyEnvelope(env, otherPub) {
		t.Fatal("verify with wrong key should return false")
	}
}

func TestVerifyEnvelope_TamperedPayload(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	env := makeTestEnvelope()
	SignEnvelope(env, priv)

	env.Payload = []byte("tampered")
	if VerifyEnvelope(env, pub) {
		t.Fatal("verify with tampered payload should return false")
	}
}

func TestVerifyEnvelope_TamperedSenderId(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	env := makeTestEnvelope()
	SignEnvelope(env, priv)

	env.SenderId = "attacker"
	if VerifyEnvelope(env, pub) {
		t.Fatal("verify with tampered sender_id should return false")
	}
}

func TestVerifyEnvelope_HopCountExcludedFromSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	env := makeTestEnvelope()
	SignEnvelope(env, priv)

	// Changing hop_count should NOT invalidate the signature
	env.HopCount = 1
	if !VerifyEnvelope(env, pub) {
		t.Fatal("hop_count change should not invalidate signature")
	}
}

func TestVerifyEnvelope_SenderPubkeyExcludedFromSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	env := makeTestEnvelope()
	SignEnvelope(env, priv)

	// Changing sender_pubkey should NOT invalidate the signature
	env.SenderPubkey = []byte("some-other-pubkey-bytes")
	if !VerifyEnvelope(env, pub) {
		t.Fatal("sender_pubkey change should not invalidate signature")
	}
}

func TestEnvelopeSignData_Deterministic(t *testing.T) {
	env := makeTestEnvelope()
	d1 := EnvelopeSignData(env)
	d2 := EnvelopeSignData(env)
	if string(d1) != string(d2) {
		t.Fatal("EnvelopeSignData should be deterministic")
	}
}

func TestEnvelopeSignData_DifferentForDifferentFields(t *testing.T) {
	env1 := makeTestEnvelope()
	env2 := makeTestEnvelope()
	env2.MsgType = 99

	d1 := EnvelopeSignData(env1)
	d2 := EnvelopeSignData(env2)
	if string(d1) == string(d2) {
		t.Fatal("different msg_type should produce different sign data")
	}
}

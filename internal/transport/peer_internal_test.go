package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	pb "micelio/pkg/proto"
)

func TestSignVerifyEnvelope(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	env := &pb.Envelope{
		MessageId: "test-msg-123",
		SenderId:  "sender-abc",
		MsgType:   MsgTypeChat,
		HopCount:  1,
		Payload:   []byte("hello"),
	}

	signEnvelope(env, priv)

	if len(env.Signature) != ed25519.SignatureSize {
		t.Fatalf("signature length: got %d, want %d", len(env.Signature), ed25519.SignatureSize)
	}

	if !verifySignature(env, pub) {
		t.Fatal("valid signature should verify")
	}
}

func TestVerifySignatureTamperedPayload(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	env := &pb.Envelope{
		MessageId: "msg-1",
		SenderId:  "sender",
		MsgType:   MsgTypeChat,
		Payload:   []byte("original"),
	}
	signEnvelope(env, priv)

	// Tamper with payload
	env.Payload = []byte("tampered")

	if verifySignature(env, pub) {
		t.Fatal("tampered payload should not verify")
	}
}

func TestVerifySignatureWrongKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	env := &pb.Envelope{
		MessageId: "msg-2",
		SenderId:  "sender",
		MsgType:   MsgTypeChat,
		Payload:   []byte("data"),
	}
	signEnvelope(env, priv)

	if verifySignature(env, otherPub) {
		t.Fatal("wrong public key should not verify")
	}
}

func TestVerifySignatureInvalidKeyLengths(t *testing.T) {
	env := &pb.Envelope{
		MessageId: "msg-3",
		SenderId:  "sender",
		MsgType:   MsgTypeChat,
		Signature: make([]byte, ed25519.SignatureSize),
	}

	// Short public key
	if verifySignature(env, []byte("short")) {
		t.Fatal("short public key should not verify")
	}

	// Short signature
	env.Signature = []byte("short")
	pub := make([]byte, ed25519.PublicKeySize)
	if verifySignature(env, pub) {
		t.Fatal("short signature should not verify")
	}
}

func TestSignEnvelopeBadPrivateKey(t *testing.T) {
	env := &pb.Envelope{
		MessageId: "msg-4",
		SenderId:  "sender",
		MsgType:   MsgTypeChat,
	}

	// Short private key should be a no-op (no panic)
	signEnvelope(env, []byte("short"))
	if env.Signature != nil {
		t.Fatal("bad private key should not produce a signature")
	}
}

func TestSignatureExcludesHopCount(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	env := &pb.Envelope{
		MessageId: "msg-5",
		SenderId:  "sender",
		MsgType:   MsgTypeChat,
		HopCount:  5,
		Payload:   []byte("data"),
	}
	signEnvelope(env, priv)

	// Change hop_count — signature should still verify (excluded from sign data)
	env.HopCount = 99
	if !verifySignature(env, pub) {
		t.Fatal("hop_count change should not invalidate signature")
	}
}

func TestEnvelopeSignDataDeterministic(t *testing.T) {
	env := &pb.Envelope{
		MessageId: "msg-det",
		SenderId:  "sender",
		MsgType:   MsgTypeChat,
		LamportTs: 42,
		Payload:   []byte("hello"),
	}

	d1 := pb.EnvelopeSignData(env)
	d2 := pb.EnvelopeSignData(env)
	if len(d1) != len(d2) {
		t.Fatal("sign data not deterministic")
	}
	for i := range d1 {
		if d1[i] != d2[i] {
			t.Fatal("sign data not deterministic")
		}
	}
}

func TestIsDuplicate(t *testing.T) {
	p := &Peer{
		seen: make(map[string]time.Time),
	}

	if p.isDuplicate("msg-a") {
		t.Fatal("first time should not be duplicate")
	}
	if !p.isDuplicate("msg-a") {
		t.Fatal("second time should be duplicate")
	}
	if p.isDuplicate("msg-b") {
		t.Fatal("different message should not be duplicate")
	}
}

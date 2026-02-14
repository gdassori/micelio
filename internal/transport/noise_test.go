package transport

import (
	"bytes"
	"net"
	"testing"

	"github.com/flynn/noise"
)

func makeNoiseKeypair(t *testing.T) noise.DHKey {
	t.Helper()
	kp, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	return kp
}

func TestHandshakeAndPeerStatic(t *testing.T) {
	keyA := makeNoiseKeypair(t)
	keyB := makeNoiseKeypair(t)

	connA, connB := net.Pipe()
	defer func() { _ = connA.Close() }()
	defer func() { _ = connB.Close() }()

	var ncA, ncB *noiseConn
	var peerKeyA, peerKeyB []byte
	var errA, errB error

	done := make(chan struct{})
	go func() {
		defer close(done)
		ncB, peerKeyB, errB = Handshake(connB, false, keyB)
	}()

	ncA, peerKeyA, errA = Handshake(connA, true, keyA)
	<-done

	if errA != nil {
		t.Fatalf("initiator handshake: %v", errA)
	}
	if errB != nil {
		t.Fatalf("responder handshake: %v", errB)
	}

	// PeerStatic should return the other side's public key
	if !bytes.Equal(ncA.PeerStatic(), keyB.Public) {
		t.Error("initiator PeerStatic should match responder public key")
	}
	if !bytes.Equal(ncB.PeerStatic(), keyA.Public) {
		t.Error("responder PeerStatic should match initiator public key")
	}
	if !bytes.Equal(peerKeyA, keyB.Public) {
		t.Error("returned peerKey A should match responder public key")
	}
	if !bytes.Equal(peerKeyB, keyA.Public) {
		t.Error("returned peerKey B should match initiator public key")
	}
}

func doHandshake(t *testing.T, keyA, keyB noise.DHKey) (*noiseConn, *noiseConn) {
	t.Helper()
	connA, connB := net.Pipe()
	t.Cleanup(func() {
		_ = connA.Close()
		_ = connB.Close()
	})

	var ncA, ncB *noiseConn
	var errA, errB error

	done := make(chan struct{})
	go func() {
		defer close(done)
		ncB, _, errB = Handshake(connB, false, keyB)
	}()
	ncA, _, errA = Handshake(connA, true, keyA)
	<-done

	if errA != nil || errB != nil {
		t.Fatalf("handshake: A=%v B=%v", errA, errB)
	}
	return ncA, ncB
}

func TestNoiseConnReadWrite(t *testing.T) {
	keyA := makeNoiseKeypair(t)
	keyB := makeNoiseKeypair(t)
	ncA, ncB := doHandshake(t, keyA, keyB)

	// Write from A, read from B
	msg := []byte("hello noise")
	go func() {
		if _, err := ncA.Write(msg); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	buf := make([]byte, 100)
	n, err := ncB.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Errorf("got %q, want %q", buf[:n], msg)
	}
}

func TestNoiseConnPartialRead(t *testing.T) {
	keyA := makeNoiseKeypair(t)
	keyB := makeNoiseKeypair(t)
	ncA, ncB := doHandshake(t, keyA, keyB)

	// Write a message larger than the read buffer
	msg := []byte("this is a longer message for partial read testing")
	go func() {
		if _, err := ncA.Write(msg); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	// Read in small chunks to exercise the readBuf path
	var result []byte
	buf := make([]byte, 10) // small buffer
	for len(result) < len(msg) {
		n, err := ncB.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		result = append(result, buf[:n]...)
	}

	if !bytes.Equal(result, msg) {
		t.Errorf("got %q, want %q", result, msg)
	}
}

func TestNoiseConnClose(t *testing.T) {
	keyA := makeNoiseKeypair(t)
	keyB := makeNoiseKeypair(t)
	ncA, ncB := doHandshake(t, keyA, keyB)

	if err := ncA.Close(); err != nil {
		t.Errorf("close A: %v", err)
	}
	if err := ncB.Close(); err != nil {
		t.Errorf("close B: %v", err)
	}
}

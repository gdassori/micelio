package transport_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"micelio/internal/transport"
)

func TestFrameRoundTrip(t *testing.T) {
	payload := []byte("hello micelio")
	var buf bytes.Buffer

	if err := transport.WriteFrame(&buf, payload); err != nil {
		t.Fatal(err)
	}

	got, err := transport.ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %q, want %q", got, payload)
	}
}

func TestFrameEmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := transport.WriteFrame(&buf, []byte{}); err != nil {
		t.Fatal(err)
	}
	got, err := transport.ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty payload, got %d bytes", len(got))
	}
}

func TestFrameInvalidMagic(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xFF, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x41})
	_, err := transport.ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

func TestFramePayloadTooLarge(t *testing.T) {
	var buf bytes.Buffer
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint16(hdr[0:2], transport.Magic)
	binary.BigEndian.PutUint16(hdr[2:4], transport.Version)
	binary.BigEndian.PutUint32(hdr[4:8], transport.MaxPayload+1)
	buf.Write(hdr)

	_, err := transport.ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected error for oversized payload")
	}
}

func TestFrameWriteTooLarge(t *testing.T) {
	payload := make([]byte, transport.MaxPayload+1)
	var buf bytes.Buffer
	err := transport.WriteFrame(&buf, payload)
	if err == nil {
		t.Fatal("expected error for oversized write")
	}
}

func TestFrameInvalidVersion(t *testing.T) {
	var buf bytes.Buffer
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint16(hdr[0:2], transport.Magic)
	binary.BigEndian.PutUint16(hdr[2:4], 0x9999) // wrong version
	binary.BigEndian.PutUint32(hdr[4:8], 1)
	buf.Write(hdr)
	buf.WriteByte(0x41) // 1-byte payload

	_, err := transport.ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected error for unsupported version")
	}
}

func TestFrameMultipleRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	messages := []string{"first", "second", "third message with more data"}

	for _, msg := range messages {
		if err := transport.WriteFrame(&buf, []byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
	}

	for _, want := range messages {
		got, err := transport.ReadFrame(&buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestFrameTruncatedHeader(t *testing.T) {
	// Only 4 bytes instead of 8 — should fail
	var buf bytes.Buffer
	buf.Write([]byte{0x4D, 0x49, 0x00, 0x01})

	_, err := transport.ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected error for truncated header")
	}
}

func TestFrameMaxPayloadExact(t *testing.T) {
	// Exactly MaxPayload should succeed
	payload := make([]byte, transport.MaxPayload)
	var buf bytes.Buffer
	if err := transport.WriteFrame(&buf, payload); err != nil {
		t.Fatalf("write MaxPayload: %v", err)
	}
	got, err := transport.ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read MaxPayload: %v", err)
	}
	if len(got) != transport.MaxPayload {
		t.Fatalf("got %d bytes, want %d", len(got), transport.MaxPayload)
	}
}

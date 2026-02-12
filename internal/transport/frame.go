package transport

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	Magic      = 0x4D49    // "MI"
	Version    = 0x0001
	MaxPayload = 1 << 20   // 1 MB
	HeaderSize = 8          // 2 (magic) + 2 (version) + 4 (length)
)

// WriteFrame writes a framed message: [2B magic][2B version][4B length][payload].
// The entire frame is written in a single Write call to minimize Noise messages.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("payload too large: %d > %d", len(payload), MaxPayload)
	}

	buf := make([]byte, HeaderSize+len(payload))
	binary.BigEndian.PutUint16(buf[0:2], Magic)
	binary.BigEndian.PutUint16(buf[2:4], Version)
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(payload)))
	copy(buf[8:], payload)

	_, err := w.Write(buf)
	return err
}

// ReadFrame reads a framed message from r, validating magic, version, and length.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("reading frame header: %w", err)
	}

	magic := binary.BigEndian.Uint16(hdr[0:2])
	if magic != Magic {
		return nil, fmt.Errorf("invalid magic: 0x%04X", magic)
	}

	version := binary.BigEndian.Uint16(hdr[2:4])
	if version != Version {
		return nil, fmt.Errorf("unsupported protocol version: %d", version)
	}

	length := binary.BigEndian.Uint32(hdr[4:8])
	if length > MaxPayload {
		return nil, fmt.Errorf("payload too large: %d > %d", length, MaxPayload)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("reading payload: %w", err)
	}

	return payload, nil
}

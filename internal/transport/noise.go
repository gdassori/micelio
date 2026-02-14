package transport

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/flynn/noise"
)

var cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

// noiseConn wraps a net.Conn with Noise transport encryption.
// It provides a transparent io.ReadWriter: the framing layer writes plaintext,
// noiseConn encrypts on write and decrypts on read.
//
// Internal wire format per message: [4B ciphertext_len][ciphertext]
type noiseConn struct {
	conn       net.Conn
	send       *noise.CipherState
	recv       *noise.CipherState
	readBuf    []byte
	writeMu    sync.Mutex
	peerStatic []byte // peer's X25519 static public key
}

// Handshake performs a Noise XX handshake over conn using the given static keypair.
// Returns the encrypted connection wrapper and the peer's X25519 static public key.
func Handshake(conn net.Conn, initiator bool, staticKey noise.DHKey) (*noiseConn, []byte, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cipherSuite,
		Pattern:       noise.HandshakeXX,
		Initiator:     initiator,
		StaticKeypair: staticKey,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("noise handshake config: %w", err)
	}

	var sendCS, recvCS *noise.CipherState

	if initiator {
		// → e
		msg1, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("noise write msg1: %w", err)
		}
		if err := writeHandshakeMsg(conn, msg1); err != nil {
			return nil, nil, err
		}

		// ← e, ee, s, es
		msg2, err := readHandshakeMsg(conn)
		if err != nil {
			return nil, nil, err
		}
		if _, _, _, err = hs.ReadMessage(nil, msg2); err != nil {
			return nil, nil, fmt.Errorf("noise read msg2: %w", err)
		}

		// → s, se
		msg3, cs1, cs2, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("noise write msg3: %w", err)
		}
		if err := writeHandshakeMsg(conn, msg3); err != nil {
			return nil, nil, err
		}
		sendCS, recvCS = cs1, cs2
	} else {
		// ← e
		msg1, err := readHandshakeMsg(conn)
		if err != nil {
			return nil, nil, err
		}
		if _, _, _, err = hs.ReadMessage(nil, msg1); err != nil {
			return nil, nil, fmt.Errorf("noise read msg1: %w", err)
		}

		// → e, ee, s, es
		msg2, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("noise write msg2: %w", err)
		}
		if err := writeHandshakeMsg(conn, msg2); err != nil {
			return nil, nil, err
		}

		// ← s, se
		msg3, err := readHandshakeMsg(conn)
		if err != nil {
			return nil, nil, err
		}
		if _, cs1, cs2, err := hs.ReadMessage(nil, msg3); err != nil {
			return nil, nil, fmt.Errorf("noise read msg3: %w", err)
		} else {
			sendCS, recvCS = cs2, cs1
		}
	}

	return &noiseConn{
		conn:       conn,
		send:       sendCS,
		recv:       recvCS,
		peerStatic: hs.PeerStatic(),
	}, hs.PeerStatic(), nil
}

// PeerStatic returns the peer's X25519 static public key from the Noise handshake.
func (nc *noiseConn) PeerStatic() []byte {
	return nc.peerStatic
}

// Write encrypts plaintext and writes it as a single Noise transport message.
func (nc *noiseConn) Write(p []byte) (int, error) {
	nc.writeMu.Lock()
	defer nc.writeMu.Unlock()

	ciphertext, err := nc.send.Encrypt(nil, nil, p)
	if err != nil {
		return 0, fmt.Errorf("noise encrypt: %w", err)
	}

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ciphertext)))

	if _, err := nc.conn.Write(lenBuf[:]); err != nil {
		return 0, err
	}
	if _, err := nc.conn.Write(ciphertext); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read decrypts a Noise transport message and returns plaintext.
func (nc *noiseConn) Read(p []byte) (int, error) {
	// Return buffered data from a previous partial read
	if len(nc.readBuf) > 0 {
		n := copy(p, nc.readBuf)
		nc.readBuf = nc.readBuf[n:]
		return n, nil
	}

	// Read one encrypted message
	var lenBuf [4]byte
	if _, err := io.ReadFull(nc.conn, lenBuf[:]); err != nil {
		return 0, err
	}
	msgLen := binary.BigEndian.Uint32(lenBuf[:])

	// Cap at MaxPayload + HeaderSize + 16-byte Poly1305 tag to prevent OOM from malicious length.
	const maxNoiseMsg = MaxPayload + HeaderSize + 16
	if msgLen > maxNoiseMsg {
		return 0, fmt.Errorf("noise message too large: %d > %d", msgLen, maxNoiseMsg)
	}

	ciphertext := make([]byte, msgLen)
	if _, err := io.ReadFull(nc.conn, ciphertext); err != nil {
		return 0, err
	}

	plaintext, err := nc.recv.Decrypt(nil, nil, ciphertext)
	if err != nil {
		return 0, fmt.Errorf("noise decrypt: %w", err)
	}

	n := copy(p, plaintext)
	if n < len(plaintext) {
		nc.readBuf = plaintext[n:]
	}
	return n, nil
}

// SetReadDeadline sets the read deadline on the underlying TCP connection.
//
// The deadline only applies when readBuf is empty, i.e. when Read would
// block on the TCP socket. For peers that use WriteFrame as implemented
// here (one complete frame per Write call), each Noise message carries
// exactly one frame, so readBuf is normally empty between ReadFrame calls.
func (nc *noiseConn) SetReadDeadline(t time.Time) error {
	return nc.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the write deadline on the underlying TCP connection.
func (nc *noiseConn) SetWriteDeadline(t time.Time) error {
	return nc.conn.SetWriteDeadline(t)
}

// Close closes the underlying connection.
func (nc *noiseConn) Close() error {
	return nc.conn.Close()
}

// Handshake message framing: [2B length][message]
func writeHandshakeMsg(w io.Writer, msg []byte) error {
	if len(msg) > 0xFFFF {
		return fmt.Errorf("handshake message too large: %d > %d", len(msg), 0xFFFF)
	}
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(msg)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("noise handshake write len: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("noise handshake write msg: %w", err)
	}
	return nil
}

func readHandshakeMsg(r io.Reader) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("noise handshake read len: %w", err)
	}
	msgLen := binary.BigEndian.Uint16(lenBuf[:])
	msg := make([]byte, msgLen)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, fmt.Errorf("noise handshake read msg: %w", err)
	}
	return msg, nil
}

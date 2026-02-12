# Transport

All peer-to-peer communication in Micelio flows over TCP connections encrypted with the Noise Protocol Framework. This page describes the two layers that sit between TCP and application messages: **Noise encryption** and **wire framing**.

## Noise Encryption

### Cipher Suite

| Parameter | Algorithm |
|-----------|-----------|
| DH | X25519 |
| Cipher | ChaChaPoly (ChaCha20-Poly1305) |
| Hash | BLAKE2s |

### Handshake Pattern: XX

Micelio uses the **Noise XX** handshake pattern. XX provides mutual authentication — both sides learn the other's static public key — without requiring either party to know the other's key in advance.

```
XX:
  → e
  ← e, ee, s, es
  → s, se
```

Three messages are exchanged:

| Step | Direction | Tokens | Purpose |
|------|-----------|--------|---------|
| 1 | Initiator → Responder | `e` | Initiator sends ephemeral public key |
| 2 | Responder → Initiator | `e, ee, s, es` | Responder sends ephemeral key, performs DH, sends static key (encrypted) |
| 3 | Initiator → Responder | `s, se` | Initiator sends static key (encrypted), performs final DH |

After the handshake completes, both sides hold:

- A pair of **CipherState** objects (one for sending, one for receiving).
- The peer's **X25519 static public key**, authenticated by the DH exchange.

### Handshake Wire Format

Each handshake message is framed as:

```
┌──────────────────┬─────────────────────┐
│ length (2 bytes) │ handshake message   │
│ big-endian       │ (variable)          │
└──────────────────┴─────────────────────┘
```

### Transport Wire Format

After the handshake, all data is encrypted. Each write produces one Noise transport message:

```
┌──────────────────┬─────────────────────┐
│ length (4 bytes) │ ciphertext          │
│ big-endian       │ (variable)          │
└──────────────────┴─────────────────────┘
```

- The **length** field is a 4-byte big-endian `uint32` encoding the ciphertext length (plaintext + 16 bytes Poly1305 tag).
- Each message is independently encrypted with a nonce derived from the message counter.

The Noise layer is transparent to the application framing above it. The application writes plaintext; the Noise layer encrypts. The application reads plaintext; the Noise layer decrypts.

## Wire Framing

On top of the Noise-encrypted channel, Micelio uses a simple frame format to delimit protobuf messages.

### Frame Format

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
┌───────────────┬───────────────┬───────────────────────────────┐
│  Magic 0x4D49 │ Version 0x0001│         Payload Length        │
│   (2 bytes)   │   (2 bytes)   │          (4 bytes)            │
├───────────────┴───────────────┴───────────────────────────────┤
│                                                               │
│                     Payload (protobuf)                        │
│                                                               │
└───────────────────────────────────────────────────────────────┘
```

| Field | Offset | Size | Encoding | Value |
|-------|--------|------|----------|-------|
| Magic | 0 | 2 bytes | Big-endian uint16 | `0x4D49` ("MI") |
| Version | 2 | 2 bytes | Big-endian uint16 | `0x0001` |
| Payload length | 4 | 4 bytes | Big-endian uint32 | 0 — 1,048,576 |
| Payload | 8 | variable | Raw bytes | Serialized protobuf `Envelope` |

**Header size:** 8 bytes.

### Constraints

| Constraint | Value | Behavior on violation |
|------------|-------|-----------------------|
| Magic mismatch | `!= 0x4D49` | Connection closed |
| Unknown version | `!= 0x0001` | Connection closed |
| Payload too large | `> 1 MB` | Connection closed |
| Empty payload | 0 bytes | Allowed |

### Write Optimization

The header and payload are concatenated into a single buffer before being passed to the Noise layer. This ensures each application message produces exactly one Noise transport message, minimizing overhead and simplifying the encrypted framing.

## Full Stack Example

A single chat message traverses the stack as follows:

```
Application:   ChatMessage{nick:"alice", text:"hello"}
                        │
                        ▼
Protobuf:      Envelope{message_id, sender_id, msg_type=3,
               payload=marshal(ChatMessage), signature}
                        │
                        ▼
Framing:       [4D 49] [00 01] [00 00 00 xx] [protobuf bytes]
                        │
                        ▼
Noise:         [00 00 00 yy] [encrypted(frame)]
                        │
                        ▼
TCP:           raw bytes on the wire
```

On the receiving side, the stack is traversed in reverse: TCP → Noise decrypt → frame parse → protobuf unmarshal → application handler.

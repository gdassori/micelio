# Identity

Every Micelio node has a persistent cryptographic identity. This identity is the foundation of authentication, message signing, and node addressing.

## Keypair

Each node generates an **ED25519** keypair on first startup. The keypair is stored on disk and reused across restarts.

| Component | Format | Size | Storage |
|-----------|--------|------|---------|
| Private key | ED25519 seed + public key | 64 bytes | `<data_dir>/identity/node.key` (PEM) |
| Public key | ED25519 | 32 bytes | `<data_dir>/identity/node.pub` (OpenSSH) |

The private key never leaves the node. The public key is transmitted during the PeerHello exchange and can be shared freely.

## Node ID

The **Node ID** is derived deterministically from the public key:

```
node_id = hex(SHA-256(ed25519_public_key))
```

This produces a 64-character lowercase hexadecimal string. For example:

```
3754c5d29f1f96f227abb4562a4d23a57ff4a64fcf11eeb97bbac6fa336aaf22
```

The Node ID is:

- **Deterministic.** The same keypair always produces the same Node ID.
- **Collision-resistant.** SHA-256 provides 128-bit collision resistance.
- **Self-certifying.** Any peer can verify a Node ID by hashing the presented public key.

## Key Derivation for Noise

The Noise Protocol Framework uses X25519 for key exchange, not ED25519. Micelio derives X25519 keys from the ED25519 identity so that a single keypair serves both purposes.

### Private Key Conversion

```
x25519_private = SHA-512(ed25519_seed)[:32]
```

X25519 applies clamping internally, so no explicit clamping step is needed.

### Public Key Conversion

The ED25519 public key (in Edwards form) is converted to X25519 (Montgomery form):

```
point = edwards25519.Point.SetBytes(ed25519_public_key)
x25519_public = point.BytesMontgomery()
```

This is a standard birational map between the two curve forms. The conversion is one-way: given an X25519 public key, you cannot recover the ED25519 public key (there are two possible preimages).

### Identity Binding

During the [PeerHello exchange](connection.md#peerhello-exchange), the peer sends its ED25519 public key. The receiver:

1. Derives X25519 from the claimed ED25519 key.
2. Compares it to the X25519 static key authenticated by the Noise handshake.
3. Rejects the connection if they don't match.

This binds the ED25519 signing identity to the Noise session, preventing an attacker from using one keypair for Noise and a different one for message signing.

# Micelio Protocol

Micelio is a pure peer-to-peer network protocol. No master, no slave, no coordinator. Nodes form a mesh, communicate via encrypted channels, and share an eventual-consistent distributed state.

Named after fungal mycelium — a decentralized network where every hypha is equal, nutrients (messages and state) propagate organically, and the network grows and adapts.

## Design Principles

- **Zero trust between nodes.** Every message is signed. Every connection is mutually authenticated. Identity is derived from cryptographic keys, not configuration.
- **No special roles.** Any node can be a center, a spoke, a bridge, or all three at once. The protocol is symmetric.
- **Encryption is mandatory.** All peer-to-peer traffic flows through Noise Protocol encrypted channels. There is no plaintext mode.
- **Minimal wire format.** An 8-byte frame header, a protobuf envelope, and Noise encryption. No HTTP, no TLS, no JSON.
- **Outbound-only nodes are first-class citizens.** Nodes behind NAT or firewalls can participate fully by dialing out. No listen port is required.

## Protocol Stack

From bottom to top:

| Layer | What | Reference |
|-------|------|-----------|
| **TCP** | Reliable, ordered byte stream | OS kernel |
| **Noise XX** | Mutual authentication + encryption | [Transport](protocol/transport.md#noise-encryption) |
| **Frame** | Message boundaries: `[magic][version][length][payload]` | [Transport](protocol/transport.md#wire-framing) |
| **Envelope** | Signed protobuf wrapper with UUID, sender, type | [Messages](protocol/messages.md#envelope) |
| **Payload** | PeerHello, ChatMessage, etc. | [Messages](protocol/messages.md#message-types) |

## Current Status

The protocol is implemented through Phase 2:

- **Phase 1** — Standalone node: identity generation, SSH partyline, bbolt storage.
- **Phase 2** — Direct connections: Noise-encrypted TCP, protobuf wire protocol, PeerHello handshake, bidirectional chat relay, message signing, deduplication.

Phases 3-7 (peer discovery, gossip, distributed state, tagging, plugins) are planned but not yet implemented. Messages currently propagate only to directly connected peers — there is no multi-hop relay.

## Quick Reference

| Property | Value |
|----------|-------|
| Identity | ED25519 keypair |
| Node ID | `hex(SHA-256(ed25519_public_key))` — 64 hex chars |
| Key exchange | Noise XX (X25519, ChaChaPoly, BLAKE2s) |
| Serialization | Protocol Buffers 3 |
| Frame magic | `0x4D49` ("MI") |
| Max payload | 1 MB |
| Signature | ED25519 over `message_id ‖ sender_id ‖ lamport_ts ‖ msg_type ‖ payload` |
| Deduplication | Per-peer seen set, keyed by `message_id`, TTL 5 min |

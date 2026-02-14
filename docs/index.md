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

The protocol is implemented through Phase 4:

- **Phase 1** — Standalone node: identity generation, SSH partyline, bbolt storage.
- **Phase 2** — Direct connections: Noise-encrypted TCP, protobuf wire protocol, PeerHello handshake, bidirectional chat relay, message signing, deduplication.
- **Phase 3** — Peer discovery: PeerExchange messages, automatic mesh formation, persistent peer table (bbolt), exponential backoff, MaxPeers enforcement, advertise address support.
- **Phase 4** — Gossip protocol: multi-hop message relay (fanout=3, max_hops=10), centralized deduplication via seen cache, auto-key-learning from `sender_pubkey`, per-sender rate limiting, trust-level keyring.
- **Connection health** — Read deadlines (60s), keepalive messages (20s), write deadlines (10s), and handshake timeout (10s) detect and evict dead or unresponsive peers.

Phases 5-7 (distributed state, tagging, plugins) are planned but not yet implemented.

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
| Idle timeout | 60 seconds (read deadline per connection) |
| Keepalive interval | 20 seconds (suppressed when real traffic flows) |
| Write timeout | 10 seconds (per-frame write deadline) |
| Handshake timeout | 10 seconds (PeerHello exchange deadline) |
| Deduplication | Gossip engine seen cache (`message_id`, TTL 5 min) + per-peer seen set for PeerExchange |

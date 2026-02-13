# Messages

All messages on the wire are serialized as Protocol Buffers 3. Every message is wrapped in an `Envelope` that provides identity, deduplication, typing, and authentication.

## Envelope

The `Envelope` is the universal wrapper for every message exchanged between peers.

```protobuf
message Envelope {
    string message_id   = 1;   // UUID v4, unique per message
    string sender_id    = 2;   // node_id of originator
    uint64 lamport_ts   = 3;   // Lamport timestamp (included in signatures; currently 0)
    uint32 hop_count    = 4;   // decremented at each hop
    uint32 msg_type     = 5;   // payload type discriminator
    bytes  payload      = 6;   // serialized inner message
    bytes  signature    = 7;   // ED25519 signature
    bytes  sender_pubkey = 8;  // raw 32-byte ED25519 public key of originator
}
```

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `message_id` | string | UUID v4. Globally unique. Used for deduplication. |
| `sender_id` | string | Node ID of the originating node (64 hex chars). |
| `lamport_ts` | uint64 | Lamport timestamp. Included in signature computation. Currently set to 0; will carry meaningful values in Phase 5 (distributed state). |
| `hop_count` | uint32 | TTL decremented at each relay hop. Messages with `hop_count <= 1` are delivered locally but not forwarded. |
| `msg_type` | uint32 | Discriminator for the `payload` contents. See [Message Types](#message-types). |
| `payload` | bytes | Serialized inner protobuf message. |
| `signature` | bytes | ED25519 signature over the signed data. |
| `sender_pubkey` | bytes | Raw 32-byte ED25519 public key of the originating node. Allows receiving nodes to verify signatures and auto-learn keys without a prior PeerHello exchange (see [Auto-Learn](#auto-learn)). |

### Signature

The signature covers all fields **except** `hop_count` (which changes at each relay hop), `signature` itself, and `sender_pubkey` (which is an auxiliary field for key distribution):

```
signed_data = message_id ‖ sender_id ‖ lamport_ts (8 bytes, big-endian) ‖ msg_type (4 bytes, big-endian) ‖ payload
```

```
signature = ED25519_Sign(private_key, signed_data)
```

Verification uses the sender's ED25519 public key, which can be learned in two ways:

1. **PeerHello exchange** — during [connection setup](connection.md#peerhello-exchange), keys are registered with `TrustDirectlyVerified`.
2. **Auto-learn from `sender_pubkey`** — when a gossip message arrives from an unknown sender, the gossip engine can learn the key from the envelope (see [Auto-Learn](#auto-learn)).

!!! note
    The `hop_count` field is deliberately excluded from the signature so that intermediate nodes can decrement it during gossip relay without invalidating the original sender's signature.

### Auto-Learn

When a gossip message arrives with an unknown `sender_id`, the gossip engine attempts to learn the sender's key from the `sender_pubkey` field:

1. `sender_pubkey` must be exactly 32 bytes (ED25519 public key size).
2. `SHA-256(sender_pubkey)` must equal `sender_id` (hex-encoded).
3. If both checks pass, the key is added to the keyring with `TrustGossipLearned` (weaker than `TrustDirectlyVerified`).
4. The envelope signature is then verified against the learned key.

This mechanism solves a bootstrap problem in gossip networks: when node A sends a message that is relayed by node B to node C, node C may not have A's public key. By including the public key in the envelope, C can verify the message immediately without waiting for a PeerExchange.

!!! note
    Auto-learned keys never overwrite directly verified keys. If a node's key is already known from a PeerHello exchange (`TrustDirectlyVerified`), the auto-learned key is ignored.

### Deduplication

The gossip engine maintains a **seen cache**: a map from `message_id` to the time it was first seen. Message processing follows a two-phase approach:

1. **Read-only check** (`Has`): if `message_id` is already in the seen cache, the message is silently dropped.
2. **Key lookup and signature verification**: the sender's key is looked up (or auto-learned), and the signature is verified.
3. **Atomic mark** (`Check`): only after verification succeeds is the `message_id` marked as seen.

This ensures that messages dropped due to an unknown sender or invalid signature are **not** marked as seen. If the key is later learned (e.g., via a subsequent PeerExchange or auto-learn), a re-delivered copy of the same message can still be processed.

Entries are evicted after a **5-minute TTL**. A cleanup goroutine runs every 60 seconds.

Additionally, each peer maintains its own per-connection seen set for PeerExchange deduplication (non-gossip messages).

## Message Types

The `msg_type` field determines how `payload` should be deserialized.

| Value | Constant | Payload type | Direction | Gossip |
|-------|----------|-------------|-----------|--------|
| 1 | `MsgTypePeerHello` | `PeerHello` | Initiator → Responder | No |
| 2 | `MsgTypePeerHelloAck` | `PeerHello` | Responder → Initiator | No |
| 3 | `MsgTypeChat` | `ChatMessage` | Bidirectional | Yes |
| 4 | `MsgTypePeerExchange` | `PeerExchange` | Bidirectional | No |
| 5-99 | — | Reserved for core protocol | — | — |
| 1000+ | — | Reserved for plugins (Phase 7) | — | — |

Messages with `msg_type` 1, 2, and 4 are handled directly by the peer connection layer. All other message types (including `MsgTypeChat`) are routed through the gossip engine for deduplication, signature verification, rate limiting, and forwarding.

## PeerHello

Exchanged immediately after the Noise handshake to establish application-level identity.

```protobuf
message PeerHello {
    string node_id        = 1;
    string version        = 2;
    repeated string tags  = 3;
    bool reachable        = 4;
    string listen_addr    = 5;
    bytes ed25519_pubkey  = 6;
}
```

The same protobuf type is used for both `PeerHello` (msg_type=1) and `PeerHelloAck` (msg_type=2). A separate `PeerHelloAck` message type is defined in the proto file for clarity but uses the same field layout.

| Field | Type | Description |
|-------|------|-------------|
| `node_id` | string | The sender's Node ID (64 hex chars). |
| `version` | string | Protocol version string (e.g., `"0.1.0"`). |
| `tags` | repeated string | Arbitrary labels for this node (e.g., `["EU", "prod"]`). |
| `reachable` | bool | `true` if this node accepts inbound connections. |
| `listen_addr` | string | The advertised `host:port` for this node. Empty if not reachable. Uses `network.advertise_addr` if set, otherwise the bound listen address (wildcard addresses like `0.0.0.0` are not advertised). |
| `ed25519_pubkey` | bytes | Raw 32-byte ED25519 public key. |

!!! important
    The `ed25519_pubkey` field is critical for [identity verification](connection.md#identity-verification). It allows the receiver to bind the ED25519 signing identity to the Noise session and verify the Node ID.

## ChatMessage

A chat message relayed through the partyline system and propagated via gossip.

```protobuf
message ChatMessage {
    string nick      = 1;
    string text      = 2;
    uint64 timestamp = 3;
}
```

| Field | Type | Description |
|-------|------|-------------|
| `nick` | string | Display name of the user who sent the message. |
| `text` | string | The message content. |
| `timestamp` | uint64 | Unix timestamp (seconds) when the message was created. |

## PeerExchange

Carries a list of known reachable peers for automatic mesh formation.

```protobuf
message PeerInfo {
    string node_id        = 1;
    string addr           = 2;
    bool   reachable      = 3;
    bytes  ed25519_pubkey = 4;
    uint64 last_seen      = 5;
}

message PeerExchange {
    repeated PeerInfo peers = 1;
}
```

| Field | Type | Description |
|-------|------|-------------|
| `node_id` | string | Node ID of the known peer (64 hex chars). |
| `addr` | string | The `host:port` the peer listens on. |
| `reachable` | bool | `true` if the peer accepts inbound connections. |
| `ed25519_pubkey` | bytes | Raw 32-byte ED25519 public key of the peer. |
| `last_seen` | uint64 | Unix timestamp (seconds) when this peer was last seen. |

### PeerExchange Flow

PeerExchange messages are sent directly between peers (hop_count=1, not gossip-relayed) in two scenarios:

1. **On connect:** Immediately after a new peer completes the PeerHello exchange and is added to the peer map, the manager sends a PeerExchange containing all known reachable peers (excluding the recipient).
2. **Periodically:** Every `exchange_interval` (default 30s), the manager sends a PeerExchange to all connected peers.

When a PeerExchange is received:

1. The peer's receive loop deserializes the `PeerExchange` payload.
2. The manager's `mergeDiscoveredPeers` method validates each `PeerInfo` entry:
    - Entries for self, non-reachable peers, empty addresses, or wildcard addresses (`0.0.0.0`, `::`) are skipped.
    - The `node_id` must equal `hex(SHA-256(ed25519_pubkey))`. Entries with mismatched IDs are rejected (prevents poisoning the peer table with invalid identities).
    - Valid entries are added to the known peers table, or updated if the `last_seen` timestamp is newer.
3. The discovery loop (every `discovery_interval`, default 10s) scans the known peers table for candidates: reachable, not already connected, not currently being dialed, not a bootstrap address (already managed by `dialWithBackoff`), and backoff elapsed.
4. Up to 3 candidates per tick are dialed with random jitter (0-2s).

### Chat Message Flow

When a local user types a message:

1. The SSH session sends it to the local **Hub**.
2. The Hub broadcasts it to all local sessions and pushes a `RemoteMsg` to the transport layer.
3. The transport **Manager** reads from the `remoteSend` channel, wraps the message in a signed `Envelope` (with `hop_count = max_hops`, `sender_pubkey` = local public key), and calls `gossip.Engine.Broadcast()`.
4. The gossip engine marks the message as seen and sends it to **all** connected peers.

When a remote chat message arrives:

1. The peer's receive loop reads the frame, deserializes the `Envelope`, and routes it to the gossip engine via the `onGossipMessage` callback.
2. The gossip engine performs [deduplication](#deduplication), key lookup (with [auto-learn](#auto-learn) if needed), signature verification, and rate limiting.
3. If valid, the `ChatMessage` handler deserializes the payload and calls `Hub.DeliverRemote(nick, text)`, which injects it as a **system message** into the local partyline.
4. If `hop_count > 1`, the engine decrements the hop count and forwards the message to a random subset of connected peers (fanout = 3), excluding the peer that delivered it.

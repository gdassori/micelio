# Messages

All messages on the wire are serialized as Protocol Buffers 3. Every message is wrapped in an `Envelope` that provides identity, deduplication, typing, and authentication.

## Envelope

The `Envelope` is the universal wrapper for every message exchanged between peers.

```protobuf
message Envelope {
    string message_id = 1;   // UUID v4, unique per message
    string sender_id  = 2;   // node_id of originator
    uint64 lamport_ts = 3;   // Lamport clock (reserved, unused in Phase 2)
    uint32 hop_count  = 4;   // decremented at each hop
    uint32 msg_type   = 5;   // payload type discriminator
    bytes  payload    = 6;   // serialized inner message
    bytes  signature  = 7;   // ED25519 signature
}
```

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `message_id` | string | UUID v4. Globally unique. Used for deduplication. |
| `sender_id` | string | Node ID of the originating node (64 hex chars). |
| `lamport_ts` | uint64 | Lamport timestamp at send time. Reserved for future use (Phase 5). |
| `hop_count` | uint32 | TTL decremented at each relay hop. Reserved for future use (Phase 4). |
| `msg_type` | uint32 | Discriminator for the `payload` contents. See [Message Types](#message-types). |
| `payload` | bytes | Serialized inner protobuf message. |
| `signature` | bytes | ED25519 signature over the signed data. |

### Signature

The signature covers all fields **except** `hop_count` (which changes at each relay hop) and `signature` itself:

```
signed_data = message_id ‖ sender_id ‖ lamport_ts (8 bytes, big-endian) ‖ msg_type (4 bytes, big-endian) ‖ payload
```

```
signature = ED25519_Sign(private_key, signed_data)
```

Verification uses the sender's ED25519 public key, which is learned during the [PeerHello exchange](connection.md#peerhello-exchange).

!!! note
    The `hop_count` field is deliberately excluded from the signature. In future phases (gossip relay), intermediate nodes will decrement `hop_count` without invalidating the original sender's signature.

### Deduplication

Each peer maintains a **seen set**: a map from `message_id` to the time it was first seen. When a message arrives:

1. If `message_id` is in the seen set, the message is silently dropped.
2. Otherwise, it is added to the seen set and processed.

Entries are evicted after a **5-minute TTL**. A cleanup goroutine runs every 60 seconds.

## Message Types

The `msg_type` field determines how `payload` should be deserialized.

| Value | Constant | Payload type | Direction |
|-------|----------|-------------|-----------|
| 1 | `MsgTypePeerHello` | `PeerHello` | Initiator → Responder |
| 2 | `MsgTypePeerHelloAck` | `PeerHello` | Responder → Initiator |
| 3 | `MsgTypeChat` | `ChatMessage` | Bidirectional |
| 1-99 | — | Reserved for core protocol | — |
| 1000+ | — | Reserved for plugins (Phase 7) | — |

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
| `listen_addr` | string | The `host:port` this node listens on. Empty if not reachable. |
| `ed25519_pubkey` | bytes | Raw 32-byte ED25519 public key. |

!!! important
    The `ed25519_pubkey` field is critical for [identity verification](connection.md#identity-verification). It allows the receiver to bind the ED25519 signing identity to the Noise session and verify the Node ID.

## ChatMessage

A chat message relayed through the partyline system.

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

### Chat Message Flow

When a local user types a message:

1. The SSH session sends it to the local **Hub**.
2. The Hub broadcasts it to all local sessions and pushes a `RemoteMsg` to the transport layer.
3. The transport **Manager** fans out the message to all connected peers via `SendChat`.
4. Each peer wraps it in a signed `Envelope` and writes it to the wire.

When a remote chat message arrives:

1. The peer's receive loop reads the frame, deserializes the `Envelope`, verifies the signature, and checks for duplicates.
2. The `ChatMessage` payload is deserialized.
3. `Hub.DeliverRemote(nick, text)` injects it as a **system message** into the local partyline.

!!! note "No re-forwarding"
    Remote messages are delivered as system messages, which are explicitly excluded from the transport fanout path. This prevents echo loops in Phase 2. Gossip relay (Phase 4) will introduce controlled re-forwarding with hop-count TTL.

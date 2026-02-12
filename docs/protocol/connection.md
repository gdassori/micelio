# Connection Lifecycle

This page describes the full lifecycle of a peer connection: from TCP dial through Noise handshake, PeerHello exchange, identity verification, steady-state operation, and disconnection.

## Overview

```
TCP Connect
    │
    ▼
MaxPeers Pre-Check (inbound only)
    │
    ▼
Noise XX Handshake (3 messages)
    │
    ▼
PeerHello / PeerHelloAck Exchange
    │
    ▼
Identity Verification (3 checks)
    │
    ▼
Duplicate / MaxPeers Check
    │
    ▼
PeerExchange (initial)
    │
    ▼
Steady State (send/recv loops)
    │
    ▼
Disconnect + Cleanup
```

## TCP Connection

Connections are established over TCP. The **initiator** (dialer) opens a TCP connection to the **responder** (listener).

- Dial timeout: **10 seconds**.
- The initiator is the node that has the responder's address in its `bootstrap` list or discovered it via PeerExchange.
- The responder accepts connections on the address specified by `network.listen`.

Nodes with no `network.listen` address are **outbound-only**: they can dial peers but cannot accept inbound connections. This is the expected mode for nodes behind NAT or firewalls.

### MaxPeers Pre-Check (Inbound Only)

For inbound connections, the responder performs a quick capacity check **before** the Noise handshake. If `MaxPeers > 0` and the node is already at capacity, the TCP connection is closed immediately to avoid wasting crypto work. This is an optimization — the definitive check happens later in `addPeer`.

## Noise Handshake

Immediately after the TCP connection is established, both sides perform a [Noise XX handshake](transport.md#handshake-pattern-xx). The `initiator` flag matches the TCP role: the dialer is the Noise initiator.

After the handshake:

- All subsequent traffic is encrypted with ChaChaPoly.
- Both sides know the peer's **X25519 static public key**, authenticated by the DH exchange.
- An attacker cannot forge, replay, or tamper with any subsequent message.

## PeerHello Exchange

The PeerHello exchange is the first application-level communication after encryption is established. It serves two purposes:

1. **Identity declaration.** Each side sends its Node ID, ED25519 public key, version, tags, and network reachability.
2. **Identity verification.** Each side verifies the peer's claimed identity against the Noise-authenticated key.

### Ordering

The exchange follows a strict initiator-first ordering to prevent deadlocks:

| Step | Initiator | Responder |
|------|-----------|-----------|
| 1 | Sends `PeerHello` (msg_type=1) | Waits for `PeerHello` |
| 2 | Waits for `PeerHelloAck` (msg_type=2) | Verifies identity, sends `PeerHelloAck` |
| 3 | Verifies identity | — |

Both the `PeerHello` and `PeerHelloAck` are wrapped in signed `Envelope` messages and transmitted through the standard [wire framing](transport.md#wire-framing).

## Identity Verification

After receiving the peer's hello message, the receiver performs three checks:

### Check 1: Node ID Derivation

```
expected_node_id = hex(SHA-256(hello.ed25519_pubkey))
assert hello.node_id == expected_node_id
```

This verifies the peer's claimed Node ID is correctly derived from the presented public key. A node cannot claim an arbitrary Node ID.

### Check 2: X25519 Key Binding

```
x25519_from_hello = EdPublicToX25519(hello.ed25519_pubkey)
x25519_from_noise = noise_handshake.peer_static_key
assert x25519_from_hello == x25519_from_noise
```

This is the critical binding step. It proves that the ED25519 key presented in the hello message belongs to the same entity that performed the Noise handshake. Without this check, an attacker could:

1. Complete a Noise handshake with their own X25519 key.
2. Present someone else's ED25519 key in the hello message.
3. Forge messages that appear signed by the impersonated node.

### Check 3: Envelope Signature

The hello envelope's signature is verified against the `ed25519_pubkey` from the hello message. This confirms the peer possesses the private key corresponding to the claimed public key.

!!! warning "All three checks are required"
    Skipping any single check creates a vulnerability:

    - Without Check 1: a node can claim any Node ID.
    - Without Check 2: an attacker can impersonate another node's signing identity.
    - Without Check 3: a node could present a stolen public key without possessing the private key.

## Duplicate and MaxPeers Check

After a successful PeerHello exchange, the manager runs `addPeer`, which performs two checks:

1. **Duplicate prevention.** If a peer with the same Node ID is already connected, the newer connection is rejected. This prevents two nodes from maintaining multiple parallel connections (which could happen when both dial each other simultaneously).
2. **MaxPeers enforcement.** If `MaxPeers > 0` and the node has reached its peer limit, the new connection is rejected. This applies to both inbound and outbound connections.

On rejection, the connection is closed and the reason is logged.

## Initial PeerExchange

Immediately after a peer is successfully added, the manager sends an initial [PeerExchange](messages.md#peerexchange) message containing all known reachable peers (excluding the recipient). This kickstarts discovery — the new peer learns about the rest of the network without waiting for the next periodic exchange tick.

The manager also updates its own known peers table with the new peer's address, reachability, and public key, resetting any prior backoff state.

## Steady State

Once the hello exchange and verification complete, the peer enters steady state with two concurrent loops:

### Send Loop

Reads from a buffered channel (`sendCh`, capacity 64) and writes framed messages to the Noise connection. Messages are queued by the manager's fanout loop when a local user sends a chat message.

If the send buffer is full, messages are dropped with a log warning. This provides backpressure without blocking the hub.

### Receive Loop

Reads framed messages from the Noise connection, deserializes the `Envelope`, and processes them:

1. **Deduplication check.** If `message_id` has been seen before, the message is dropped.
2. **Signature verification.** The envelope signature is verified against the peer's ED25519 key.
3. **Sender verification.** The envelope's `sender_id` must match the peer's Node ID.
4. **Dispatch by type:**
    - `MsgTypeChat` (3): Deserialize `ChatMessage`, deliver to local Hub.
    - `MsgTypePeerExchange` (4): Deserialize `PeerExchange`, merge into known peers table.
    - Unknown types are logged and dropped.

### Seen Set Cleanup

A background goroutine runs every 60 seconds, removing entries from the seen set that are older than 5 minutes. This bounds memory usage while maintaining deduplication across reasonable network delays.

## Disconnection

A peer connection ends when either the send loop or receive loop encounters an error (typically a read/write failure on the underlying TCP connection). The sequence is:

1. The first loop to fail returns an error.
2. `Peer.Close()` is called, which closes the `done` channel and the Noise connection.
3. The second loop detects the closure and exits.
4. `Manager.removePeer()` removes the peer from the active peer map.
5. If this was a bootstrap peer (outbound connection), `dialWithBackoff` will attempt to reconnect.

### Reconnection

Outbound connections to bootstrap peers use exponential backoff:

| Attempt | Delay |
|---------|-------|
| 1st retry | 1 second |
| 2nd retry | 2 seconds |
| 3rd retry | 4 seconds |
| 4th retry | 8 seconds |
| 5th retry | 16 seconds |
| 6th+ retry | 30 seconds (max) |

After a successful connection that later disconnects, the backoff resets to 1 second. This provides fast recovery from transient failures while avoiding aggressive retry storms during extended outages.

# Architecture

This page describes the internal architecture of a Micelio node: how the components are wired together, how messages flow, and the concurrency model.

## Component Diagram

```
┌──────────────────────────────────────────────────────────────┐
│                          Node                                │
│                                                              │
│  ┌────────────┐       ┌────────────┐       ┌─────────────┐  │
│  │ SSH Server │──────▶│  Partyline │◀──────│  Transport   │  │
│  │            │◀──────│    Hub     │──────▶│   Manager    │  │
│  └────────────┘       └────────────┘       └──────┬──────┘  │
│        │                                          │          │
│        │                                   ┌──────┴──────┐   │
│        │                                   │   Gossip    │   │
│        │                                   │   Engine    │   │
│        │                                   └──────┬──────┘   │
│        │                                   ┌──────┴──────┐   │
│        │                                   │   Peer(s)   │   │
│        │                                   │ ┌─────────┐ │   │
│        │                                   │ │ Noise   │ │   │
│        │                                   │ │ Conn    │ │   │
│        │                                   │ └─────────┘ │   │
│        │                                   └─────────────┘   │
│        │                                                     │
│  ┌─────┴──────┐  ┌────────────┐  ┌────────────┐             │
│  │  Identity  │  │   Store    │  │   Config   │             │
│  │  ED25519   │  │   bbolt   │  │   TOML     │             │
│  └────────────┘  └────────────┘  └────────────┘             │
└──────────────────────────────────────────────────────────────┘
```

## Components

### Identity

Generates and loads an ED25519 keypair. Provides the Node ID and an SSH host signer. Created once at startup, shared read-only with all other components.

**Source:** `internal/identity/`

### Config

Parses TOML configuration with sensible defaults. CLI flags override file values. Shared read-only after initialization.

**Source:** `internal/config/`

### Store

Abstract key-value interface backed by bbolt (embedded B+ tree). Provides bucket-scoped Get/Set/Delete/ForEach/Snapshot operations. Currently used for persistent storage; will be used for distributed state in Phase 5.

**Source:** `internal/store/`, `internal/store/bolt/`

### Partyline Hub

The central message broker for the node. Manages user sessions and broadcasts messages using a **channel-only architecture** (no mutexes). A single goroutine owns the session map; all operations are serialized through channels.

**Source:** `internal/partyline/`

Operations:

| Channel | Purpose |
|---------|---------|
| `join` | Register a new session |
| `leave` | Unregister a session |
| `broadcast` | Send a message to all sessions |
| `who` | List active session nicknames |
| `setNick` | Change a session's nickname |
| `remoteSend` | Forward local user messages to transport (outbound) |

The Hub also provides `DeliverRemote(nick, text)` for incoming remote messages. These are injected as **system messages** (flagged `system: true`) so they are displayed locally but never re-forwarded to the transport layer.

### SSH Server

Provides human access to the partyline via SSH. Handles public key authentication (against `authorized_keys`), terminal allocation, and session management.

**Source:** `internal/ssh/`

### Gossip Engine

The gossip protocol engine. Handles deduplication, signature verification via a trust-level keyring, per-sender rate limiting, hop count enforcement, local delivery to registered message handlers, and random-subset forwarding (fanout = 3, max_hops = 10).

**Source:** `internal/gossip/`

Sub-components:

| Component | Purpose |
|-----------|---------|
| `SeenCache` | Deduplication via message ID tracking with 5-minute TTL |
| `KeyRing` | Maps node IDs to ED25519 public keys with trust levels (`TrustDirectlyVerified` = 0, `TrustGossipLearned` = 1) |
| `RateLimiter` | Per-sender token bucket (10 msg/s, burst = 2x rate) |

The engine supports auto-learning sender keys from the `sender_pubkey` envelope field, allowing verification of messages from nodes not yet directly connected (see [Auto-Learn](protocol/messages.md#auto-learn)).

### Transport Manager

Manages all peer connections. Responsibilities:

- **Listening** for inbound TCP connections (if `network.listen` is configured). Inbound connections are rejected early (before handshake) when at `MaxPeers` capacity.
- **Dialing** bootstrap peers with exponential backoff.
- **Noise handshake** and PeerHello exchange for each connection.
- **Address advertising:** determines the address to advertise to peers. Prefers `network.advertise_addr` if set; otherwise uses the bound listen address, but suppresses wildcard addresses (`0.0.0.0`, `::`) since peers cannot reach them.
- **Fanout loop:** reads from the Hub's `remoteSend` channel, wraps messages in signed envelopes (with `sender_pubkey`), and broadcasts via the gossip engine.
- **Gossip integration:** creates the gossip engine, registers message handlers, registers peer public keys with `TrustDirectlyVerified` during PeerHello, and routes incoming gossip messages from peers to the engine.
- **Peer lifecycle:** adds/removes peers, prevents duplicates, enforces `MaxPeers`.
- **Peer discovery:** exchanges known peers via PeerExchange messages and auto-dials discovered peers (excluding bootstrap addresses, which are managed separately by `dialWithBackoff`).

**Source:** `internal/transport/`

### Discovery

Automatic peer discovery and mesh formation. The Manager maintains a `knownPeers` table (in-memory, optionally persisted to bbolt) that tracks all known peers with their addresses, reachability, public keys, and backoff state.

Two loops drive discovery:

- **Exchange loop** (`exchange_interval`, default 30s): periodically sends PeerExchange messages to all connected peers, sharing the list of known reachable peers.
- **Discovery loop** (`discovery_interval`, default 10s): scans `knownPeers` for dial candidates — peers that are reachable, not already connected, not currently being dialed, not a bootstrap address, and whose backoff has elapsed. Up to 3 candidates per tick are dialed with random jitter. Bootstrap addresses are excluded because `dialWithBackoff` already handles their reconnection lifecycle.

Incoming PeerInfo entries are validated: the `node_id` must equal `hex(SHA-256(ed25519_pubkey))`. Entries with mismatched IDs, wildcard addresses (`0.0.0.0`, `::`), or empty addresses are rejected to prevent peer table poisoning.

Exponential backoff (`min(2^(failCount-1), 30)` seconds) prevents connection storms to unreachable peers. Backoff resets on successful connection. Peer records are persisted to bbolt (if available) so discovery state survives restarts.

**Source:** `internal/transport/discovery.go`

### Peer

Represents a single connection to a remote node. Each Peer runs:

- A **send loop** that reads from a buffered channel and writes framed messages.
- A **receive loop** that reads framed messages, deserializes envelopes, and dispatches by message type:
    - `PeerHello`/`PeerHelloAck` (msg_type=1,2): silently dropped — handshake messages are point-to-point only and must never be gossip-relayed.
    - `PeerExchange` (msg_type=4): per-peer deduplication, signature verification against the peer's known key, then routed to the manager's `onPeerExchange` callback.
    - All other types: routed to the gossip engine via `onGossipMessage` for centralized dedup, key verification, rate limiting, and forwarding.
- A **seen set cleanup** goroutine for per-peer PeerExchange deduplication.

**Source:** `internal/transport/peer.go`

## Message Flow

### Local User → Remote Peers

```
User types "hello" in SSH terminal
        │
        ▼
SSH session calls Hub.Broadcast(session, "hello")
        │
        ▼
Hub.Run() goroutine:
  1. Formats "[alice] hello"
  2. Sends to all local sessions (except sender)
  3. Pushes RemoteMsg{Nick:"alice", Text:"hello"} to remoteSend channel
        │
        ▼
Manager.fanoutLoop() goroutine:
  1. Reads RemoteMsg from channel
  2. Wraps in Envelope (UUID, sender_id, msg_type=3, hop_count=max_hops,
     sender_pubkey=local_public_key)
  3. Signs with ED25519 private key
  4. Calls gossip.Engine.Broadcast(envelope)
        │
        ▼
Gossip Engine:
  1. Marks message_id as seen
  2. Serializes envelope
  3. Sends to ALL connected peers (originator sends to all, not fanout subset)
        │
        ▼
Peer.sendLoop() goroutine:
  Reads from sendCh → WriteFrame → noiseConn.Write → TCP
```

### Remote Peer → Local Users

```
TCP bytes arrive
        │
        ▼
noiseConn.Read() → decrypt → plaintext
        │
        ▼
ReadFrame() → validate magic, version, length → payload bytes
        │
        ▼
Peer.recvLoop():
  1. Unmarshal Envelope protobuf
  2. Route by msg_type:
     - PeerExchange (4) → per-peer dedup + verify + handlePeerExchange
     - All others → onGossipMessage callback
        │
        ▼
Gossip Engine (HandleIncoming):
  0. Reject malformed envelopes (empty message_id or sender_id)
  1. Read-only dedup check (Has) — drop if already seen
  2. Key lookup in keyring; auto-learn from sender_pubkey if unknown
  3. Verify ED25519 signature
  4. Rate limit check (before marking seen, so rate-limited messages
     can be retried when the sender's budget replenishes)
  5. Atomic mark as seen (Check) — only after verification and rate
     limiting succeed
  6. Deliver to registered handler:
     → ChatMessage handler → Hub.DeliverRemote(nick, text) → local sessions
  7. Forward if hop_count > 1: decrement, re-serialize, send to random
     subset of peers (fanout=3, excluding sender)
```

## Concurrency Model

| Goroutine | Lifetime | Owned State |
|-----------|----------|-------------|
| `Hub.Run()` | Node lifetime | Session map |
| `Manager.Start()` | Node lifetime (blocks on ctx) | — |
| `Manager.listenLoop()` | Node lifetime | — |
| `Manager.fanoutLoop()` | Node lifetime | — |
| `Manager.exchangeLoop()` | Node lifetime | — |
| `Manager.discoveryLoop()` | Node lifetime | — |
| `Manager.dialWithBackoff()` | One per bootstrap addr | Backoff timer |
| `Manager.dialDiscovered()` | Per-discovered-peer dial attempt | — |
| `Gossip.SeenCache.CleanupLoop()` | Node lifetime | Seen cache eviction |
| `Gossip.RateLimiter.CleanupLoop()` | Node lifetime | Rate limiter bucket eviction |
| `Peer.sendLoop()` | Per-connection | — |
| `Peer.recvLoop()` | Per-connection | — |
| `Peer.cleanupSeenLoop()` | Per-connection | Per-peer seen set cleanup |
| SSH session handler | Per-SSH-connection | Terminal state |

The Manager's peer map is protected by a `sync.Mutex`. The Hub uses no mutexes — all state is owned by the `Run()` goroutine and accessed through channels. The Peer's per-connection seen set is protected by a `sync.Mutex`. The gossip engine's `SeenCache`, `KeyRing`, and `RateLimiter` each use their own `sync.Mutex`. The Noise connection's write path is protected by a `sync.Mutex` (although in practice only the send loop writes). Both `Engine.Stop()` and `Peer.Close()` use `sync.Once` to ensure idempotent shutdown (safe to call multiple times without double-close panics).

## Topology Examples

### Star

```
  spoke1 ──→ center ←── spoke2
  spoke3 ──↗         ↖── spoke4
```

All spokes bootstrap to the center. Messages from the center reach all spokes. Messages from a spoke reach the center, which relays them to other spokes via gossip (fanout = 3).

### Bridge (outbound-only node connecting two networks)

```
  Net A              Net B
  a1 → a0 ←── bridge ──→ b0 ← b1
  a2 ↗                      ↖ b2
```

The bridge has no listen port. It dials both centers. Messages from any node can cross networks via gossip relay through the bridge. For example, a message from a1 reaches a0, which forwards to bridge, which forwards to b0, which delivers to b1 and b2.

### Auto-Mesh via Discovery (Phase 3)

```
  A ←──→ B ←──→ C
  │              │
  └──────────────┘  (discovered via PeerExchange)
```

Nodes that share a common bootstrap peer will discover each other via PeerExchange messages and automatically form direct connections. For example: A bootstraps to B, C bootstraps to B; B tells A about C and vice versa; A and C connect directly, forming a full mesh.

Discovery also works transitively across longer chains:

```
  A → B → C → D → E  (bootstrap chain)
         ↓
  A ←→ B ←→ C ←→ D ←→ E
  │    │    │    │    │
  └────┴────┴────┴────┘  (full mesh via PeerExchange)
```

PeerExchange propagates peer info along the chain. Within a few exchange+discovery cycles, all nodes discover each other and form direct connections, regardless of how far apart they were in the initial topology.

### Full Mesh with Gossip (Phase 4)

```
  A ←──→ B
  │ ╲  ╱ │
  │  ╲╱  │
  │  ╱╲  │
  │ ╱  ╲ │
  C ←──→ D
```

With gossip relay, a message from A propagates to B, C, and D through hop-count-limited flooding. The originator sends to all connected peers; each relay node forwards to a random subset (fanout = 3) of its peers, excluding the sender. The `hop_count` field limits propagation depth (default max_hops = 10). Deduplication via the seen cache prevents infinite loops.

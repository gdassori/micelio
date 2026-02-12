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

### Transport Manager

Manages all peer connections. Responsibilities:

- **Listening** for inbound TCP connections (if `network.listen` is configured). Inbound connections are rejected early (before handshake) when at `MaxPeers` capacity.
- **Dialing** bootstrap peers with exponential backoff.
- **Noise handshake** and PeerHello exchange for each connection.
- **Address advertising:** determines the address to advertise to peers. Prefers `network.advertise_addr` if set; otherwise uses the bound listen address, but suppresses wildcard addresses (`0.0.0.0`, `::`) since peers cannot reach them.
- **Fanout loop:** reads from the Hub's `remoteSend` channel and distributes messages to all connected peers.
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
- A **receive loop** that reads framed messages, verifies signatures, deduplicates, and dispatches to handlers.
- A **seen set cleanup** goroutine for deduplication TTL.

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
  Reads RemoteMsg from channel
  For each connected peer: peer.SendChat("alice", "hello")
        │
        ▼
Peer.SendChat():
  1. Marshal ChatMessage protobuf
  2. Wrap in Envelope (UUID, sender_id, msg_type=3)
  3. Sign envelope with ED25519 private key
  4. Marshal envelope, push to sendCh
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
  2. Check seen set (dedup by message_id)
  3. Verify ED25519 signature
  4. Verify sender_id matches peer
  5. Dispatch by msg_type:
     - MsgTypeChat (3) → handleChat
     - MsgTypePeerExchange (4) → handlePeerExchange
        │
        ▼
Peer.handleChat():
  1. Unmarshal ChatMessage from payload
  2. Call Hub.DeliverRemote(nick, text)
        │
        ▼
Hub.Run() goroutine:
  Broadcasts "[alice] hello" as system message to all local sessions
        │
        ▼
SSH session sender goroutine:
  Writes "[alice] hello\r\n" to terminal
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
| `Peer.sendLoop()` | Per-connection | — |
| `Peer.recvLoop()` | Per-connection | — |
| `Peer.cleanupSeenLoop()` | Per-connection | Seen set cleanup |
| SSH session handler | Per-SSH-connection | Terminal state |

The Manager's peer map is protected by a `sync.Mutex`. The Hub uses no mutexes — all state is owned by the `Run()` goroutine and accessed through channels. The Peer's seen set is protected by a `sync.Mutex`. The Noise connection's write path is protected by a `sync.Mutex` (although in practice only the send loop writes).

## Topology Examples

### Star (current typical setup)

```
  spoke1 ──→ center ←── spoke2
  spoke3 ──↗         ↖── spoke4
```

All spokes bootstrap to the center. Messages from the center reach all spokes. Messages from a spoke reach only the center.

### Bridge (outbound-only node connecting two networks)

```
  Net A              Net B
  a1 → a0 ←── bridge ──→ b0 ← b1
  a2 ↗                      ↖ b2
```

The bridge has no listen port. It dials both centers. Messages from the bridge reach both centers. Messages do not cross networks (no gossip relay in Phase 2).

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

### Future: Full Mesh with Gossip (Phase 4)

```
  A ←──→ B
  │ ╲  ╱ │
  │  ╲╱  │
  │  ╱╲  │
  │ ╱  ╲ │
  C ←──→ D
```

With gossip relay, a message from A will propagate to B, C, and D through hop-count-limited flooding. This is not yet implemented.

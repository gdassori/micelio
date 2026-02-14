# Micelio — In-Depth Codebase Analysis

Comprehensive analysis of the Micelio project: a pure P2P system written in Go implementing
a decentralized mesh network with gossip protocol, Noise-encrypted transport, and SSH
partyline interface.

**Codebase statistics:**
- 48 Go files, ~12,000 lines of production code, ~7,600 lines of tests
- 26 test files (test-to-source ratio: 63%)
- 8 direct dependencies, zero CGO dependencies
- Current phase: 4/10 (Gossip Protocol)

---

## 1. Security

### 1.1 Strengths

The codebase demonstrates solid security practices:

- **Modern cryptography**: ED25519 for identity and signatures, Noise XX (X25519 + ChaChaPoly1305 + BLAKE2s) for transport, `crypto/rand` for all key generation
- **Comprehensive input validation**: envelopes validated for empty fields, max field length (128 bytes), max payload 64KB, network addresses validated in configuration
- **Correct file permissions**: private keys saved with `0600`, directories with `0700`
- **No hardcoded secrets**: keys are generated and persisted to disk, configuration comes from TOML files or environment variables
- **No injection vectors**: no use of `eval()`, `exec()`, `unsafe`, SQL, or dynamic command execution
- **Documented logging policy**: explicitly prohibits logging of private keys, passwords, and message contents
- **Safe deserialization**: protobuf with error handling and size limits

### 1.2 Potential Vulnerabilities

#### MEDIUM — No rate limiting on SSH connections
**File:** `internal/ssh/server.go`

The SSH server accepts connections without limits. An attacker could open hundreds of
simultaneous SSH connections causing resource exhaustion. There is no limit on the number
of concurrent clients nor per-client rate limiting.

**Recommendation:** Add a maximum concurrent connections limit and per-source-IP rate
limiting.

#### LOW — Automatic peer key learning
**File:** `internal/gossip/gossip.go:183-189`

The system automatically learns peer public keys if the pubkey hash matches the sender_id.
This is cryptographically correct but could be exploited in MITM attack scenarios if the
first contact occurs over an unauthenticated channel.

**Recommendation:** Clearly document the trust model and consider a key pinning mechanism
for known peers.

#### LOW — `os.Exit()` in goroutine
**File:** `cmd/micelio/main.go:139`

```go
go func() {
    if err := sshServer.Start(ctx); err != nil {
        slog.Error("fatal: ssh", "err", err)
        os.Exit(1) // bypasses all defer cleanup blocks
    }
}()
```

Calling `os.Exit()` from a goroutine bypasses all `defer` blocks in main, preventing
orderly cleanup (database closure, peer disconnection).

**Recommendation:** Propagate the error to the main goroutine via an error channel.

---

## 2. Scalability

### 2.1 Architecture

The pure P2P architecture is inherently horizontally scalable: each node is autonomous
and the network grows by adding peers. The gossip protocol with fanout=3 and max hop=10
allows message propagation in O(log N) hops.

### 2.2 Critical Scalability Issues

#### HIGH — Unbounded maps that grow without limits

| Map | File | Problem |
|-----|------|---------|
| `KeyRing.keys` | `internal/gossip/keyring.go` | Grows with every unique peer encountered, no cleanup, no TTL |
| `knownPeers` | `internal/transport/manager.go` | Dead peers are never removed, only added |
| `seen` cache | `internal/gossip/seen.go` | Can grow up to (msg_rate × 5min) between cleanup cycles |
| `senders` rate limiter | `internal/gossip/ratelimit.go` | Stale entries accumulate until cleanup (1 min) |

In a network with 10,000 nodes and 1,000 msg/sec, the seen cache could reach ~300,000
entries between cleanups.

**Recommendation:** Implement LRU caches with maximum size. Add dead peer eviction based
on failure count and last_seen.

#### HIGH — Single-goroutine bottleneck in Partyline Hub
**File:** `internal/partyline/partyline.go:78-141`

The hub processes all operations (join, leave, broadcast, who, setNick) sequentially on
a single goroutine. With a broadcast channel buffer of 64, messages are silently dropped
under heavy load.

**Recommendation:** For the current use case (chat) this is acceptable. For more intensive
future uses, consider channel sharding or a worker pool.

#### MEDIUM — Single mutex in Transport Manager
**File:** `internal/transport/manager.go:35`

A single `sync.Mutex` protects the peer map, known peers, and dialing map. Discovery
merge operations (`discovery.go:360-384`) hold the lock during potentially long iterations.

**Recommendation:** Consider `sync.RWMutex` for reads or separate locks for different
data structures.

#### MEDIUM — bbolt single-writer
**File:** `internal/store/bolt/bolt.go`

bbolt allows only one writer at a time. Fire-and-forget `saveKnownPeer` operations
(`discovery.go:145`) can queue up if storage is slow.

**Recommendation:** Batch writes for discovery operations. Consider an asynchronous write
buffer.

#### MEDIUM — Peer exchange with full copy
**File:** `internal/transport/discovery.go:247-264`

Every 30 seconds, a full snapshot of all known peers is created, with in-memory allocation
and serial hex decoding for each peer. With 10,000 known peers, this operation becomes
expensive.

**Recommendation:** Limit the number of peers in the exchange (e.g., top-N by last_seen).

### 2.3 Current Scale Limits

- `MaxPeers`: 15 (default) — limits direct connections
- `Fanout`: 3 — only 3 peers receive each forwarded message
- `MaxHops`: 10 — reachability limited to 10 hops
- Channel buffers: 64 elements — no backpressure, silent drop

---

## 3. Code Efficiency

### 3.1 Performance Issues

#### MEDIUM — Inefficient discovery candidate selection
**File:** `internal/transport/discovery.go:396-463`

```go
// Copies ALL PeerRecords, then shuffles the entire array,
// then takes only the first 3 (maxDialsPerTick)
var candidates []*PeerRecord
for _, rec := range m.knownPeers {
    copied := *rec
    candidates = append(candidates, &copied)
}
rand.Shuffle(len(candidates), ...)
```

With 10,000 known peers and only 3 dials per tick, 9,997 records are copied and shuffled
unnecessarily.

**Recommendation:** Use random sampling (selection of N random indices) instead of a full
shuffle.

#### MEDIUM — Memory copies in Bolt store
**File:** `internal/store/bolt/bolt.go:32-34, 78-80`

Every read from Bolt forces a value copy. The `Snapshot()` method duplicates ALL values
of a bucket entry-by-entry.

**Recommendation:** For `Snapshot()` consider streaming or lazy iteration. The copies in
`Get()` are necessary for bbolt safety (values are only valid within the transaction).

#### MEDIUM — Duplicate per-peer dedup cache
**File:** `internal/transport/peer.go:71-72, 389-417`

Each peer maintains its own deduplication cache with linear cleanup every minute, in
addition to the global gossip engine cache. With 15 peers and 1,000 messages each, that
is 15 linear scans of 1,000 entries every minute.

**Recommendation:** Centralize deduplication in the gossip engine or limit per-peer size
with LRU.

#### LOW — Lock held during allocation in KeyRing
**File:** `internal/gossip/keyring.go:43-54`

The `Add()` method holds the lock during `make()` and `copy()` of the public key. In
high-throughput contexts, this can slow down concurrent reads.

**Recommendation:** Pre-copy the key outside the lock, then insert under lock.

#### LOW — Fire-and-forget goroutines without tracking
**File:** `internal/transport/discovery.go:145, 388-392`

```go
go m.saveKnownPeer(&snapshot)  // fire-and-forget
go func(recs []PeerRecord) {   // fire-and-forget batch
    for i := range recs {
        m.saveKnownPeer(&recs[i])
    }
}(toPersist)
```

Without cancellation context or tracking, if `saveKnownPeer` blocks, these goroutines
accumulate.

**Recommendation:** Integrate with the lifecycle context. Use a worker pool or buffered
channel to serialize saves.

---

## 4. Code Quality

### 4.1 Strengths

- **Idiomatic Go patterns**: channel-based concurrency, interface segregation, table-driven tests
- **Zero-mutex partyline design**: fully channel-based, prevents deadlocks and race conditions
- **Clean dependency injection**: callback pattern for gossip handlers, interface for store
- **Abstract storage**: `store.Store` interface allows swapping bbolt for other backends
- **Comprehensive tests**: multi-node integration tests, discovery tests, gossip tests with statistics
- **Consistent naming**: short receivers (`h`, `e`, `m`, `p`), descriptive functions, well-named constants
- **Inline documentation**: comments on design decisions, documented invariants

### 4.2 Quality Issues

#### CRITICAL — No panic recovery in goroutines

7+ goroutines without recovery:

| Goroutine | File | Line |
|-----------|------|------|
| `hub.Run()` | `cmd/micelio/main.go` | 112 |
| `mgr.Start()` | `cmd/micelio/main.go` | 115 |
| `sshServer.Start()` | `cmd/micelio/main.go` | 136 |
| `listenLoop()` | `internal/transport/manager.go` | 118 |
| `exchangeLoop()` | `internal/transport/manager.go` | 125 |
| `discoveryLoop()` | `internal/transport/manager.go` | 127 |
| `cleanupSeenLoop()` | `internal/transport/peer.go` | 148 |

A panic in any of these goroutines crashes the entire node.

**Recommendation:** Create a `safeGo` wrapper:
```go
func safeGo(logger *slog.Logger, fn func()) {
    go func() {
        defer func() {
            if r := recover(); r != nil {
                logger.Error("goroutine panic recovered",
                    "panic", r,
                    "stack", string(debug.Stack()))
            }
        }()
        fn()
    }()
}
```

#### MEDIUM — Cleanup errors silently ignored

In several places in the codebase, `Close()` errors are ignored with `_ = `:
- `internal/transport/manager.go`: 8 occurrences
- `internal/ssh/server.go`: 11 occurrences
- `internal/transport/peer.go`: 3 occurrences

While most of these are cleanup operations where little can be done, some (like
`listener.Close()`) could indicate resource exhaustion.

**Recommendation:** Log at least at WARN level for Close errors on listeners and main
connections.

#### LOW — No benchmark tests

No benchmark tests (`func BenchmarkXxx`) are present. For a P2P system where performance
is critical, benchmarks would be useful for:
- Gossip message throughput
- Noise handshake latency
- bbolt read/write throughput
- Seen cache lookup/cleanup
- Peer exchange serialization

#### LOW — Context not propagated to Partyline Hub
**File:** `internal/partyline/partyline.go:78`

`Hub.Run()` does not accept `context.Context`. It uses an internal `done` channel but
does not integrate with the application's context tree.

---

## 5. Priority Summary

### High Priority
| # | Issue | Area | Impact |
|---|-------|------|--------|
| 1 | No panic recovery in goroutines | Quality | Complete node crash |
| 2 | Unbounded maps (KeyRing, knownPeers) | Scalability | Slow memory leak |
| 3 | Missing SSH rate limiting | Security | Potential DoS |

### Medium Priority
| # | Issue | Area | Impact |
|---|-------|------|--------|
| 4 | Single mutex in Transport Manager | Scalability | Lock contention |
| 5 | Discovery candidate selection O(N) | Performance | CPU waste |
| 6 | Duplicate per-peer dedup cache | Performance | Memory and CPU |
| 7 | Peer exchange with full snapshot | Scalability | Bandwidth and memory |
| 8 | os.Exit() in goroutine | Quality | Missed cleanup |
| 9 | bbolt single-writer bottleneck | Scalability | Serialized I/O |

### Low Priority
| # | Issue | Area | Impact |
|---|-------|------|--------|
| 10 | Lock during allocation in KeyRing | Performance | Minor lock contention |
| 11 | Fire-and-forget goroutines | Quality | Potential goroutine leak |
| 12 | No benchmark tests | Quality | Missing baseline |
| 13 | Silently ignored cleanup errors | Quality | Difficult debugging |

---

## 6. Conclusion

Micelio is a well-structured project with solid foundations: modern cryptography, correct
P2P architecture, idiomatic Go patterns, and comprehensive tests. No critical security
vulnerabilities were found.

The main risks concern **long-term scalability** (unbounded maps, single-thread bottlenecks)
and **operational resilience** (missing panic recovery). These issues are typical of a
project in active development (phase 4 of 10) and can be addressed without significant
rewrites.

The architectural foundations — particularly the abstract storage, callback-based dependency
injection, and channel-based design — provide a good base for tackling scale challenges
in subsequent phases of the project.

# Micelio

[![CI](https://github.com/gdassori/micelio/actions/workflows/ci.yml/badge.svg)](https://github.com/gdassori/micelio/actions/workflows/ci.yml)
[![Coverage Status](https://coveralls.io/repos/github/gdassori/micelio/badge.svg?branch=master)](https://coveralls.io/github/gdassori/micelio?branch=master)

A pure P2P system of standalone programs. No master, no slave, no coordinator. Nodes form a mesh, communicate via gossip, share an eventual-consistent distributed state, and are extensible through plugins. Every node exposes an SSH partyline for human interaction.

Named after fungal mycelium — a decentralized network where every hypha is equal, nutrients (messages/state) propagate organically, and the network grows and adapts.

## Architecture

```
┌─────────────────────────────────────────────┐
│                  Node                       │
│                                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  │
│  │ SSH      │  │ Partyline│  │ State    │  │
│  │ Server   │──│ Hub      │──│ LWW Map  │  │
│  └──────────┘  └──────────┘  └──────────┘  │
│                      │                      │
│               ┌──────────┐                  │
│               │ Gossip   │                  │
│               └──────────┘                  │
│                    │                        │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  │
│  │ Transport│──│ Noise    │──│ Peer     │  │
│  │ Manager  │  │ Encrypt  │  │ Discovery│  │
│  └──────────┘  └──────────┘  └──────────┘  │
│                                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  │
│  │ Identity │  │ Store    │  │ Plugins  │  │
│  │ ED25519  │  │ bbolt    │  │ Registry │  │
│  └──────────┘  └──────────┘  └──────────┘  │
└─────────────────────────────────────────────┘
```

## Stack

- **Language:** Go
- **Serialization:** Protocol Buffers
- **Transport:** TCP + Noise Protocol Framework
- **Storage:** bbolt (embedded B+ tree)
- **SSH:** `golang.org/x/crypto/ssh`
- **Build:** Static binaries, zero external dependencies, `CGO_ENABLED=0`

## Build

```bash
# Requires Go 1.26+
make build              # → bin/micelio

make release            # Cross-compile all targets:
                        #   bin/micelio-linux-amd64
                        #   bin/micelio-linux-arm64
                        #   bin/micelio-linux-386
                        #   bin/micelio-darwin-arm64
```

## Run

```bash
# Start a node (generates identity on first run)
bin/micelio

# Override defaults via flags
bin/micelio -ssh-listen 127.0.0.1:2222 -name my-node -data-dir ~/.micelio

# Connect two nodes
bin/micelio -name node-a -ssh-listen 127.0.0.1:2222 -config nodeA.toml
bin/micelio -name node-b -ssh-listen 127.0.0.1:2223 -config nodeB.toml
```

On first run, the node creates `~/.micelio/` with:
```
~/.micelio/
├── config.toml         # (optional) TOML configuration
├── identity/
│   ├── node.key        # ED25519 private key
│   └── node.pub        # ED25519 public key (OpenSSH format)
├── authorized_keys     # SSH public keys allowed to connect
└── data.db             # bbolt database
```

## Connect

```bash
# Add your SSH key
cat ~/.ssh/id_ed25519.pub >> ~/.micelio/authorized_keys

# Connect to the partyline
ssh -p 2222 localhost
```

## Configuration

```toml
[node]
name = "node-eu-1"
tags = ["EU", "prod"]
data_dir = "~/.micelio"

[ssh]
listen = "0.0.0.0:2222"

[network]
listen = "0.0.0.0:4000"
advertise_addr = "203.0.113.10:4000"  # explicit public address (optional)
bootstrap = [
    "seed1.example.com:4000",
    "seed2.example.com:4000"
]
max_peers = 15
exchange_interval = "30s"   # peer exchange period
discovery_interval = "10s"  # discovery scan period
```

CLI flags take precedence over config file values.

## Partyline Commands

| Command | Description |
|---------|-------------|
| `/who` | List connected users |
| `/nick <name>` | Change nickname |
| `/help` | Show available commands |
| `/quit` | Disconnect |

## Project Structure

```
micelio/
├── cmd/micelio/           # Entry point
├── internal/
│   ├── config/            # TOML config + CLI flags
│   ├── crypto/            # ED25519→X25519 key conversion
│   ├── identity/          # ED25519 keypair, NodeID
│   ├── partyline/         # Chat hub (channel-based, zero mutexes)
│   ├── ssh/               # SSH server, terminal sessions
│   ├── store/             # Store interface + bbolt implementation
│   └── transport/         # Noise encryption, wire framing, peer management, discovery
├── pkg/proto/             # Generated protobuf Go code
├── proto/                 # Protobuf definitions
├── docs/                  # Protocol documentation (MkDocs)
├── Makefile
└── go.mod
```

## Roadmap

- [x] **Phase 1** — Standalone node with SSH partyline
- [x] **Phase 2** — Two nodes, direct connection ([#1](../../issues/1))
- [x] **Phase 3** — Peer discovery and mesh formation ([#2](../../issues/2))
- [ ] **Phase 4** — Gossip protocol ([#3](../../issues/3))
- [ ] **Phase 5** — Distributed state map with LWW + Lamport clock ([#4](../../issues/4))
- [ ] **Phase 6** — Tagging and desired state ([#5](../../issues/5))
- [ ] **Phase 7** — Plugin system ([#6](../../issues/6))

Each phase produces a working, testable binary. Each phase adds a layer without rewriting the ones below.

## License

MIT

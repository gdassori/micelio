# Micelio Logging Policy

This document defines the logging conventions for the micelio project.
All contributors (human or AI) must follow these standards.

## Library

We use Go's stdlib `log/slog` exclusively — no external logging dependencies.
The thin wrapper in `internal/logging/` provides:

- `logging.Init(level, format)` — call once at startup to configure the global logger.
- `logging.For(component)` — returns `*slog.Logger` with a `"component"` attribute.
- `logging.SetLevel(slog.Level)` — change level at runtime (used in tests).
- `logging.CaptureForTest(t)` — test helper that captures log records for assertions.

## Log Levels

| Level   | Purpose                                      | Examples                                           |
|---------|----------------------------------------------|----------------------------------------------------|
| `ERROR` | Unrecoverable failures; something is broken  | Marshal failure, store write failure                |
| `WARN`  | Anomalous but recoverable; sub-optimal state | Invalid signature, handshake failed, buffer full    |
| `INFO`  | Important system events; rare, high-signal   | Node started, peer connected/disconnected, listening |
| `DEBUG` | Verbose diagnostics; used in tests           | Message received/forwarded, dial failed, key learned |

**Rules:**
- INFO must not flood — if a log line fires more than once per minute in normal operation, it belongs at DEBUG.
- ERROR means the operation failed and could not recover. If we can continue, use WARN.
- DEBUG is where most new log lines should go. When in doubt, use DEBUG.
- Never log at WARN or ERROR in a hot path (per-message processing). Use DEBUG.

## Component Names

Each package gets a component logger with a short, lowercase name:

| Package                    | Component      | Variable  |
|----------------------------|----------------|-----------|
| `cmd/micelio`              | *(none)*       | `slog.*`  |
| `internal/gossip`          | `"gossip"`     | `logger`  |
| `internal/transport`       | `"transport"`  | `tlog`    |
| `internal/transport` (discovery) | `"discovery"` | `dlog` |
| `internal/ssh`             | `"ssh"`        | `sshlog`  |
| `internal/partyline`       | `"partyline"`  | `plog`    |

New packages should follow this pattern: `var xlog = logging.For("component")`.

## Structured Attributes

Use key-value pairs, not format strings:

```go
// Good
logger.Info("peer connected", "peer", nodeID, "direction", "inbound")

// Bad
logger.Info(fmt.Sprintf("peer connected: %s (inbound)", nodeID))
```

Common attribute keys:

| Key         | Type     | Description                    |
|-------------|----------|--------------------------------|
| `"err"`     | `error`  | Error value                    |
| `"peer"`    | `string` | Remote node ID (short form OK) |
| `"addr"`    | `string` | Network address                |
| `"remote"`  | `any`    | Remote address from net.Conn   |
| `"msg_id"`  | `string` | Envelope message ID            |
| `"sender"`  | `string` | Sender node ID                 |
| `"count"`   | `int`    | Numeric count                  |
| `"nick"`    | `string` | Partyline nickname             |
| `"user"`    | `string` | SSH username                   |
| `"direction"` | `string` | `"inbound"` or `"outbound"` |

## Configuration

```toml
[logging]
level = "info"    # error, warn, info, debug
format = "text"   # text or json
```

Priority: CLI flag `-log-level` > config file > env `MICELIO_LOG_LEVEL` > default `"info"`.

## Test Assertions

Use the capture helper to verify logging behavior in tests:

```go
capture := logging.CaptureForTest(t)
defer capture.Restore()

// ... exercise code ...

if !capture.Has(slog.LevelWarn, "invalid signature") {
    t.Error("expected warning about invalid signature")
}
```

This is preferred over checking stderr output. The capture helper sets log
level to DEBUG so all messages are visible regardless of global config.

## What NOT to Log

- User-facing terminal output (`fmt.Fprintf` to SSH terminal) is NOT logging.
- Errors returned to callers — let the caller decide whether to log.
- Sensitive data (private keys, passwords, full message contents).

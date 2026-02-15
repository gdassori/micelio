package main

import (
	"fmt"
	"sort"
	"strings"

	"micelio/internal/ssh"
	"micelio/internal/state"
)

func registerStateCommands(reg ssh.CommandRegistrar, stateMap *state.Map, localNodeID string) {
	if stateMap == nil {
		return
	}

	reg.Register("/state", ssh.Command{
		Help:    "display all state entries",
		Handler: handleState(stateMap),
	})

	reg.Register("/set", ssh.Command{
		Usage:   "/set <key> <value>",
		Help:    "set a state entry (propagates via gossip)",
		Handler: handleSet(stateMap),
	})

	reg.Register("/del", ssh.Command{
		Usage:   "/del <key>",
		Help:    "delete a state entry (propagates via gossip)",
		Handler: handleDel(stateMap),
	})

	reg.Register("/get", ssh.Command{
		Usage:   "/get <key>",
		Help:    "get a state entry (plain key = own namespace, key with / = full key)",
		Handler: handleGet(stateMap, localNodeID),
	})
}

func handleState(stateMap *state.Map) func(ssh.CommandContext) bool {
	return func(ctx ssh.CommandContext) bool {
		entries := stateMap.Snapshot()
		active := entries[:0]
		for _, e := range entries {
			if !e.Deleted {
				active = append(active, e)
			}
		}
		if len(active) == 0 {
			_, _ = fmt.Fprintln(ctx.Terminal, "State: (empty)")
			return false
		}
		sort.Slice(active, func(i, j int) bool {
			return active[i].Key < active[j].Key
		})
		_, _ = fmt.Fprintf(ctx.Terminal, "State (%d entries):\r\n", len(active))
		for _, e := range active {
			_, _ = fmt.Fprintf(ctx.Terminal, "  %-20s = %s  (ts=%d node=%s)\r\n",
				e.Key, string(e.Value), e.LamportTs, shortNodeID(e.NodeID))
		}
		return false
	}
}

func handleSet(stateMap *state.Map) func(ssh.CommandContext) bool {
	return func(ctx ssh.CommandContext) bool {
		if len(ctx.Args) < 2 {
			_, _ = fmt.Fprintln(ctx.Terminal, "Usage: /set <key> <value>")
			return false
		}
		key := ctx.Args[0]
		value := strings.Join(ctx.Args[1:], " ")
		entry, accepted, err := stateMap.Set(key, []byte(value))
		if err != nil {
			_, _ = fmt.Fprintf(ctx.Terminal, "Error: %v\r\n", err)
		} else if accepted {
			_, _ = fmt.Fprintf(ctx.Terminal, "Set %s = %s (ts=%d)\r\n", key, value, entry.LamportTs)
		} else {
			_, _ = fmt.Fprintln(ctx.Terminal, "Set rejected (stale clock)")
		}
		return false
	}
}

func handleDel(stateMap *state.Map) func(ssh.CommandContext) bool {
	return func(ctx ssh.CommandContext) bool {
		if len(ctx.Args) == 0 {
			_, _ = fmt.Fprintln(ctx.Terminal, "Usage: /del <key>")
			return false
		}
		key := ctx.Args[0]
		entry, accepted, err := stateMap.Delete(key)
		if err != nil {
			_, _ = fmt.Fprintf(ctx.Terminal, "Error: %v\r\n", err)
		} else if accepted {
			_, _ = fmt.Fprintf(ctx.Terminal, "Deleted %s (ts=%d)\r\n", key, entry.LamportTs)
		} else {
			_, _ = fmt.Fprintln(ctx.Terminal, "Delete rejected (stale clock)")
		}
		return false
	}
}

func handleGet(stateMap *state.Map, localNodeID string) func(ssh.CommandContext) bool {
	return func(ctx ssh.CommandContext) bool {
		if len(ctx.Args) == 0 {
			_, _ = fmt.Fprintln(ctx.Terminal, "Usage: /get <key>")
			return false
		}
		key := ctx.Args[0]
		if !strings.Contains(key, "/") {
			key = localNodeID + "/" + key
		}
		entry, ok := stateMap.Get(key)
		if !ok {
			_, _ = fmt.Fprintf(ctx.Terminal, "%s: not found\r\n", key)
			return false
		}
		_, _ = fmt.Fprintf(ctx.Terminal, "%s = %s  (ts=%d node=%s)\r\n",
			key, string(entry.Value), entry.LamportTs, shortNodeID(entry.NodeID))
		return false
	}
}

func shortNodeID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

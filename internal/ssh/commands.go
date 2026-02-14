package ssh

import (
	"fmt"
	"strings"
	"sync"

	"micelio/internal/partyline"

	"golang.org/x/term"
)

// CommandContext holds the state available to command handlers.
type CommandContext struct {
	Session  *partyline.Session
	Terminal *term.Terminal
	Hub      *partyline.Hub
	Args     []string
}

// CommandHandler processes a partyline command. Returns true if the session
// should be closed (e.g., /quit). The caller (runTerminal) is responsible
// for calling hub.Leave; handlers must NOT call it directly.
type CommandHandler func(ctx CommandContext) bool

// Command describes a registered partyline command.
type Command struct {
	Usage   string // full usage for help (e.g., "/nick <name>"); defaults to command name
	Help    string
	Handler CommandHandler
}

// CommandRegistrar is the interface for registering commands before the server starts.
type CommandRegistrar interface {
	Register(name string, cmd Command)
	RegisterBuiltins()
}

// CommandRegistry maps command names to handlers and produces dynamic help.
// It is safe for concurrent use; Dispatch and HelpText may be called from
// multiple goroutines (e.g., concurrent SSH sessions).
// Once frozen (via Freeze), no new commands can be registered.
type CommandRegistry struct {
	mu       sync.RWMutex
	commands map[string]Command
	order    []string // insertion order for stable help output
	frozen   bool
}

// NewCommandRegistry creates an empty registry.
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{
		commands: make(map[string]Command),
	}
}

// Register adds a command to the registry. The name should include the leading
// slash (e.g., "/quit"). Registering the same name twice overwrites the previous entry.
// Panics if cmd.Handler is nil or if the registry is frozen.
func (r *CommandRegistry) Register(name string, cmd Command) {
	if cmd.Handler == nil {
		panic("ssh: Register called with nil handler for " + name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic("ssh: Register called on frozen registry for " + name)
	}
	if _, exists := r.commands[name]; !exists {
		r.order = append(r.order, name)
	}
	r.commands[name] = cmd
}

// Freeze prevents further command registration. This is called automatically
// when the server starts listening. Calling Register on a frozen registry panics.
func (r *CommandRegistry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
}

// Dispatch parses a command line and calls the matching handler.
// Returns true if the session should be closed.
func (r *CommandRegistry) Dispatch(line string, session *partyline.Session, terminal *term.Terminal, hub *partyline.Hub) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false
	}
	name := parts[0]
	args := parts[1:]

	r.mu.RLock()
	cmd, ok := r.commands[name]
	r.mu.RUnlock()

	if !ok {
		_, _ = fmt.Fprintf(terminal, "Unknown command: %s (try /help)\r\n", name)
		return false
	}

	return cmd.Handler(CommandContext{
		Session:  session,
		Terminal: terminal,
		Hub:      hub,
		Args:     args,
	})
}

// HelpText returns a formatted help string listing all registered commands
// in registration order.
func (r *CommandRegistry) HelpText() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var b strings.Builder
	b.WriteString("Commands:\n")
	for _, name := range r.order {
		cmd := r.commands[name]
		display := name
		if cmd.Usage != "" {
			display = cmd.Usage
		}
		_, _ = fmt.Fprintf(&b, "  %-14s — %s\n", display, cmd.Help)
	}
	return b.String()
}

// RegisterBuiltins registers the default partyline commands:
// /who, /nick, /quit, and /help.
func (r *CommandRegistry) RegisterBuiltins() {
	r.Register("/who", Command{
		Help: "list online users",
		Handler: func(ctx CommandContext) bool {
			nicks := ctx.Hub.Who()
			_, _ = fmt.Fprintf(ctx.Terminal, "Online (%d): %s\r\n", len(nicks), strings.Join(nicks, ", "))
			return false
		},
	})

	r.Register("/nick", Command{
		Usage: "/nick <name>",
		Help:  "change your nickname",
		Handler: func(ctx CommandContext) bool {
			if len(ctx.Args) == 0 {
				_, _ = fmt.Fprintln(ctx.Terminal, "Usage: /nick <name>")
				return false
			}
			newNick := ctx.Args[0]
			ctx.Hub.SetNick(ctx.Session, newNick)
			ctx.Terminal.SetPrompt(fmt.Sprintf("[%s]> ", newNick))
			return false
		},
	})

	r.Register("/quit", Command{
		Help: "disconnect",
		Handler: func(ctx CommandContext) bool {
			_, _ = fmt.Fprintln(ctx.Terminal, "Goodbye.")
			return true
		},
	})

	r.Register("/help", Command{
		Help: "show this help",
		Handler: func(ctx CommandContext) bool {
			_, _ = fmt.Fprint(ctx.Terminal, r.HelpText())
			return false
		},
	})
}

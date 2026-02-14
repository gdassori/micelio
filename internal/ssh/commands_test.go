package ssh

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"micelio/internal/partyline"

	"golang.org/x/term"
)

// mockTerminal creates a term.Terminal backed by an in-memory pipe.
// Returns the terminal and a function that reads all written output.
func mockTerminal(t *testing.T) (*term.Terminal, func() string) {
	t.Helper()
	r, w, err := pipePair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	t.Cleanup(func() { _ = w.Close() })
	terminal := term.NewTerminal(readWriter{r, w}, "> ")
	readOutput := func() string {
		_ = w.Close()
		data, _ := io.ReadAll(r)
		return string(data)
	}
	return terminal, readOutput
}

func TestRegistryDispatchKnown(t *testing.T) {
	hub := partyline.NewHub("test")
	go hub.Run()
	defer hub.Stop()

	session := hub.Join("alice")
	defer hub.Leave(session)

	terminal, readOutput := mockTerminal(t)

	var called bool
	reg := NewCommandRegistry()
	reg.Register("/ping", Command{
		Help: "test command",
		Handler: func(ctx CommandContext) bool {
			called = true
			if ctx.Hub != hub {
				t.Error("Hub mismatch")
			}
			if ctx.Session != session {
				t.Error("Session mismatch")
			}
			if len(ctx.Args) != 1 || ctx.Args[0] != "pong" {
				t.Errorf("Args: got %v, want [pong]", ctx.Args)
			}
			return false
		},
	})

	exit := reg.Dispatch("/ping pong", session, terminal, hub)
	_ = readOutput()

	if !called {
		t.Error("handler was not called")
	}
	if exit {
		t.Error("expected exit=false")
	}
}

func TestRegistryDispatchUnknown(t *testing.T) {
	hub := partyline.NewHub("test")
	go hub.Run()
	defer hub.Stop()

	session := hub.Join("bob")
	defer hub.Leave(session)

	terminal, readOutput := mockTerminal(t)

	reg := NewCommandRegistry()
	exit := reg.Dispatch("/nope", session, terminal, hub)
	out := readOutput()

	if exit {
		t.Error("expected exit=false for unknown command")
	}
	if !strings.Contains(out, "Unknown command: /nope") {
		t.Errorf("expected unknown command message, got: %q", out)
	}
}

func TestRegistryDispatchExit(t *testing.T) {
	hub := partyline.NewHub("test")
	go hub.Run()
	defer hub.Stop()

	session := hub.Join("carol")
	defer hub.Leave(session)

	terminal, readOutput := mockTerminal(t)

	reg := NewCommandRegistry()
	reg.Register("/exit", Command{
		Help:    "exit",
		Handler: func(_ CommandContext) bool { return true },
	})

	exit := reg.Dispatch("/exit", session, terminal, hub)
	_ = readOutput()

	if !exit {
		t.Error("expected exit=true")
	}
}

func TestRegistryHelpText(t *testing.T) {
	reg := NewCommandRegistry()
	reg.RegisterBuiltins()

	help := reg.HelpText()

	if !strings.Contains(help, "Commands:") {
		t.Error("help should start with 'Commands:'")
	}

	for _, cmd := range []string{"/who", "/nick", "/quit", "/help"} {
		if !strings.Contains(help, cmd) {
			t.Errorf("help should contain %q", cmd)
		}
	}

	// /help should be last
	lines := strings.Split(strings.TrimSpace(help), "\n")
	lastLine := lines[len(lines)-1]
	if !strings.Contains(lastLine, "/help") {
		t.Errorf("last line should be /help, got: %q", lastLine)
	}
}

func TestRegistryHelpDynamic(t *testing.T) {
	reg := NewCommandRegistry()
	reg.RegisterBuiltins()
	reg.Register("/custom", Command{Help: "a custom command", Handler: func(_ CommandContext) bool { return false }})

	help := reg.HelpText()
	if !strings.Contains(help, "/custom") {
		t.Error("help should include dynamically registered /custom")
	}
	if !strings.Contains(help, "a custom command") {
		t.Error("help should include custom command description")
	}
}

func TestBuiltins(t *testing.T) {
	tests := []struct {
		name    string
		nick    string
		cmd     string
		wantOut []string
		wantExit bool
		leavesHub bool // handler calls Hub.Leave (skip auto-leave)
	}{
		{"who", "dave", "/who", []string{"Online (1)", "dave"}, false, false},
		{"nick_no_args", "eve", "/nick", []string{"Usage: /nick <name>"}, false, false},
		{"quit", "frank", "/quit", []string{"Goodbye"}, true, true},
		{"help", "grace", "/help", []string{"Commands:", "/who"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := partyline.NewHub("test")
			go hub.Run()
			defer hub.Stop()

			session := hub.Join(tt.nick)
			if !tt.leavesHub {
				defer hub.Leave(session)
			}

			terminal, readOutput := mockTerminal(t)

			reg := NewCommandRegistry()
			reg.RegisterBuiltins()

			exit := reg.Dispatch(tt.cmd, session, terminal, hub)
			out := readOutput()

			if exit != tt.wantExit {
				t.Errorf("exit: got %v, want %v", exit, tt.wantExit)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("expected %q in output, got: %q", want, out)
				}
			}
		})
	}
}

func TestBuiltinNickChange(t *testing.T) {
	hub := partyline.NewHub("test")
	go hub.Run()
	defer hub.Stop()

	session := hub.Join("eve")
	defer hub.Leave(session)

	terminal, readOutput := mockTerminal(t)

	reg := NewCommandRegistry()
	reg.RegisterBuiltins()

	exit := reg.Dispatch("/nick nova", session, terminal, hub)
	_ = readOutput()

	if exit {
		t.Error("expected exit=false")
	}

	nicks := hub.Who()
	if len(nicks) != 1 || nicks[0] != "nova" {
		t.Errorf("expected nick 'nova', got %v", nicks)
	}
}

func TestRegisterNilHandlerPanics(t *testing.T) {
	reg := NewCommandRegistry()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for nil handler")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "/boom") {
			t.Errorf("unexpected panic value: %v", r)
		}
	}()
	reg.Register("/boom", Command{Help: "should panic"})
}

func TestRegistryOverwrite(t *testing.T) {
	reg := NewCommandRegistry()
	var called int
	reg.Register("/test", Command{Help: "v1", Handler: func(_ CommandContext) bool { called = 1; return false }})
	reg.Register("/test", Command{Help: "v2", Handler: func(_ CommandContext) bool { called = 2; return false }})

	hub := partyline.NewHub("test")
	go hub.Run()
	defer hub.Stop()

	session := hub.Join("hal")
	defer hub.Leave(session)

	terminal, readOutput := mockTerminal(t)

	reg.Dispatch("/test", session, terminal, hub)
	_ = readOutput()

	if called != 2 {
		t.Errorf("expected overwritten handler (2), got %d", called)
	}

	// Should not duplicate in order
	help := reg.HelpText()
	if strings.Count(help, "/test") != 1 {
		t.Errorf("/test should appear once in help, got:\n%s", help)
	}
}

func TestRegistryFreeze(t *testing.T) {
	reg := NewCommandRegistry()
	reg.Register("/before", Command{Help: "registered before freeze", Handler: func(_ CommandContext) bool { return false }})

	reg.Freeze()

	// Attempting to register after freeze should panic
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when registering on frozen registry")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "frozen") {
			t.Errorf("unexpected panic value: %v", r)
		}
	}()
	reg.Register("/after", Command{Help: "should panic", Handler: func(_ CommandContext) bool { return false }})
}

func TestRegistryConcurrentDispatch(t *testing.T) {
	hub := partyline.NewHub("test")
	go hub.Run()
	defer hub.Stop()

	reg := NewCommandRegistry()
	var counter int
	var mu sync.Mutex
	reg.Register("/count", Command{
		Help: "increment counter",
		Handler: func(_ CommandContext) bool {
			mu.Lock()
			counter++
			mu.Unlock()
			return false
		},
	})

	// Dispatch from 100 concurrent goroutines
	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			session := hub.Join(fmt.Sprintf("user%d", id))
			defer hub.Leave(session)

			terminal, readOutput := mockTerminal(t)
			reg.Dispatch("/count", session, terminal, hub)
			_ = readOutput()
		}(i)
	}

	wg.Wait()

	if counter != goroutines {
		t.Errorf("expected counter=%d, got %d", goroutines, counter)
	}
}

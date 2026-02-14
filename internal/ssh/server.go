package ssh

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"micelio/internal/identity"
	"micelio/internal/logging"
	"micelio/internal/partyline"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

var sshlog = logging.For("ssh")

// Server is an SSH server that exposes the partyline to connected users.
type Server struct {
	addr     string
	id       *identity.Identity
	hub      *partyline.Hub
	authKeys []gossh.PublicKey
	config   *gossh.ServerConfig
	listener net.Listener

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// NewServer creates an SSH server. authKeysPath points to an authorized_keys
// file in OpenSSH format. If the file doesn't exist, the server starts but
// rejects all connections.
func NewServer(addr string, id *identity.Identity, hub *partyline.Hub, authKeysPath string) (*Server, error) {
	s := &Server{
		addr:  addr,
		id:    id,
		hub:   hub,
		conns: make(map[net.Conn]struct{}),
	}

	s.authKeys = loadAuthorizedKeys(authKeysPath)
	if len(s.authKeys) == 0 {
		sshlog.Warn("no authorized keys loaded", "path", authKeysPath)
	}

	s.config = &gossh.ServerConfig{
		PublicKeyCallback: s.publicKeyCallback,
	}
	s.config.AddHostKey(id.SSHSigner)

	return s, nil
}

// Listen binds the server socket. Call Serve to start accepting connections.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.addr, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	return nil
}

// Addr returns the listener's address. Useful when listening on :0.
func (s *Server) Addr() string {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	if ln == nil {
		return ""
	}
	return ln.Addr().String()
}

// Serve accepts SSH connections until ctx is cancelled. Call Listen first.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	if ln == nil {
		return fmt.Errorf("Serve called before Listen")
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			sshlog.Warn("accept error", "err", err)
			continue
		}

		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		go s.handleConnection(conn)
	}
}

// Start is a convenience that calls Listen + Serve.
func (s *Server) Start(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve(ctx)
}

// Stop closes the listener and all active connections.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	for conn := range s.conns {
		_ = conn.Close()
	}
}

func (s *Server) removeConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *Server) publicKeyCallback(meta gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
	keyBytes := key.Marshal()
	for _, authorized := range s.authKeys {
		if bytes.Equal(keyBytes, authorized.Marshal()) {
			return &gossh.Permissions{}, nil
		}
	}
	return nil, fmt.Errorf("unknown public key for %s", meta.User())
}

func (s *Server) handleConnection(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	defer s.removeConn(conn)

	sshConn, chans, reqs, err := gossh.NewServerConn(conn, s.config)
	if err != nil {
		sshlog.Warn("handshake failed", "remote", conn.RemoteAddr(), "err", err)
		return
	}
	defer func() { _ = sshConn.Close() }()

	sshlog.Info("client connected", "remote", conn.RemoteAddr(), "user", sshConn.User())
	go gossh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(gossh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChan.Accept()
		if err != nil {
			sshlog.Warn("channel accept error", "err", err)
			continue
		}
		go s.handleSession(channel, requests, sshConn)
	}
}

func (s *Server) handleSession(ch gossh.Channel, reqs <-chan *gossh.Request, conn *gossh.ServerConn) {
	defer func() { _ = ch.Close() }()

	// Wait for pty-req and shell before starting the terminal.
	// Drain other requests in the background once shell is received.
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "shell":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			go func() {
				for req := range reqs {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
				}
			}()
			s.runTerminal(ch, conn)
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (s *Server) runTerminal(ch gossh.Channel, conn *gossh.ServerConn) {
	nick := conn.User()
	terminal := term.NewTerminal(ch, fmt.Sprintf("[%s]> ", nick))

	session := s.hub.Join(nick)

	// Sender goroutine: hub messages → terminal
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range session.Send {
			_, _ = fmt.Fprintln(terminal, msg)
		}
	}()

	_, _ = fmt.Fprintf(terminal, "Welcome to %s partyline!\r\n", s.hub.NodeName())
	_, _ = fmt.Fprintln(terminal, "Type /help for commands.")
	_, _ = fmt.Fprintln(terminal, "")

	// Read loop: terminal → hub
	for {
		line, err := terminal.ReadLine()
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if s.handleCommand(line, session, terminal) {
				return // /quit
			}
			continue
		}
		s.hub.Broadcast(session, line)
	}

	s.hub.Leave(session)
	<-done
}

func (s *Server) handleCommand(line string, session *partyline.Session, terminal *term.Terminal) bool {
	parts := strings.Fields(line)
	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "/quit":
		_, _ = fmt.Fprintln(terminal, "Goodbye.")
		s.hub.Leave(session)
		return true

	case "/who":
		nicks := s.hub.Who()
		_, _ = fmt.Fprintf(terminal, "Online (%d): %s\r\n", len(nicks), strings.Join(nicks, ", "))

	case "/nick":
		if len(args) == 0 {
			_, _ = fmt.Fprintln(terminal, "Usage: /nick <name>")
			return false
		}
		newNick := args[0]
		s.hub.SetNick(session, newNick)
		terminal.SetPrompt(fmt.Sprintf("[%s]> ", newNick))

	case "/help":
		_, _ = fmt.Fprintln(terminal, "Commands:")
		_, _ = fmt.Fprintln(terminal, "  /who          — list online users")
		_, _ = fmt.Fprintln(terminal, "  /nick <name>  — change your nickname")
		_, _ = fmt.Fprintln(terminal, "  /quit         — disconnect")
		_, _ = fmt.Fprintln(terminal, "  /help         — show this help")

	default:
		_, _ = fmt.Fprintf(terminal, "Unknown command: %s (try /help)\r\n", cmd)
	}

	return false
}

func loadAuthorizedKeys(path string) []gossh.PublicKey {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var keys []gossh.PublicKey
	for len(data) > 0 {
		key, _, _, rest, err := gossh.ParseAuthorizedKey(data)
		if err != nil {
			break
		}
		keys = append(keys, key)
		data = rest
	}
	return keys
}

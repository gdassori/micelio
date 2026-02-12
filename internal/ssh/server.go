package ssh

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"micelio/internal/identity"
	"micelio/internal/partyline"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

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
		log.Printf("WARNING: no authorized keys loaded from %s — nobody can authenticate", authKeysPath)
	}

	s.config = &gossh.ServerConfig{
		PublicKeyCallback: s.publicKeyCallback,
	}
	s.config.AddHostKey(id.SSHSigner)

	return s, nil
}

// Listen binds the server socket. Call Serve to start accepting connections.
func (s *Server) Listen() error {
	var err error
	s.listener, err = net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.addr, err)
	}
	return nil
}

// Addr returns the listener's address. Useful when listening on :0.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Serve accepts SSH connections until ctx is cancelled. Call Listen first.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			log.Printf("ssh accept: %v", err)
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
	if s.listener != nil {
		s.listener.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.conns {
		conn.Close()
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
	defer conn.Close()
	defer s.removeConn(conn)

	sshConn, chans, reqs, err := gossh.NewServerConn(conn, s.config)
	if err != nil {
		log.Printf("ssh handshake from %s: %v", conn.RemoteAddr(), err)
		return
	}
	defer sshConn.Close()

	log.Printf("ssh connection from %s (user: %s)", conn.RemoteAddr(), sshConn.User())
	go gossh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(gossh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChan.Accept()
		if err != nil {
			log.Printf("ssh channel accept: %v", err)
			continue
		}
		go s.handleSession(channel, requests, sshConn)
	}
}

func (s *Server) handleSession(ch gossh.Channel, reqs <-chan *gossh.Request, conn *gossh.ServerConn) {
	defer ch.Close()

	// Wait for pty-req and shell before starting the terminal.
	// Drain other requests in the background once shell is received.
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if req.WantReply {
				req.Reply(true, nil)
			}
		case "shell":
			if req.WantReply {
				req.Reply(true, nil)
			}
			go func() {
				for req := range reqs {
					if req.WantReply {
						req.Reply(false, nil)
					}
				}
			}()
			s.runTerminal(ch, conn)
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
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
			fmt.Fprintln(terminal, msg)
		}
	}()

	fmt.Fprintf(terminal, "Welcome to %s partyline!\r\n", s.hub.NodeName())
	fmt.Fprintln(terminal, "Type /help for commands.")
	fmt.Fprintln(terminal, "")

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
		fmt.Fprintln(terminal, "Goodbye.")
		s.hub.Leave(session)
		return true

	case "/who":
		nicks := s.hub.Who()
		fmt.Fprintf(terminal, "Online (%d): %s\r\n", len(nicks), strings.Join(nicks, ", "))

	case "/nick":
		if len(args) == 0 {
			fmt.Fprintln(terminal, "Usage: /nick <name>")
			return false
		}
		newNick := args[0]
		s.hub.SetNick(session, newNick)
		terminal.SetPrompt(fmt.Sprintf("[%s]> ", newNick))

	case "/help":
		fmt.Fprintln(terminal, "Commands:")
		fmt.Fprintln(terminal, "  /who          — list online users")
		fmt.Fprintln(terminal, "  /nick <name>  — change your nickname")
		fmt.Fprintln(terminal, "  /quit         — disconnect")
		fmt.Fprintln(terminal, "  /help         — show this help")

	default:
		fmt.Fprintf(terminal, "Unknown command: %s (try /help)\r\n", cmd)
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

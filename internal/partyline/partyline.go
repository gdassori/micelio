package partyline

import (
	"fmt"
	"sync/atomic"
)

// RemoteMsg is a chat message destined for the P2P transport layer.
type RemoteMsg struct {
	Nick string
	Text string
}

// Hub manages partyline sessions using channels only (no mutexes).
// A single goroutine owns the session map; all operations go through channels.
type Hub struct {
	nodeName string

	join      chan joinReq
	leave     chan *Session
	broadcast chan bcastMsg
	who       chan whoReq
	setNick   chan nickReq
	stop      chan struct{}

	remoteSend chan<- RemoteMsg // set by transport; nil if no P2P
}

// Session represents a connected user in the partyline.
type Session struct {
	ID   uint64
	Nick string
	Send chan string // messages to display to this session
}

type joinReq struct {
	nick   string
	result chan *Session
}

type whoReq struct {
	result chan []string
}

type nickReq struct {
	session *Session
	nick    string
	result  chan string // previous nick
}

type bcastMsg struct {
	from   *Session // nil for system messages
	text   string
	system bool
}

var sessionCounter atomic.Uint64

// NewHub creates a new partyline hub. Call Run() in a goroutine to start it.
func NewHub(nodeName string) *Hub {
	return &Hub{
		nodeName:  nodeName,
		join:      make(chan joinReq),
		leave:     make(chan *Session),
		broadcast: make(chan bcastMsg, 64),
		who:       make(chan whoReq),
		setNick:   make(chan nickReq),
		stop:      make(chan struct{}),
	}
}

// Run is the hub's main loop. It owns the session map and processes
// all operations sequentially. Run blocks until Stop() is called.
func (h *Hub) Run() {
	sessions := make(map[uint64]*Session)

	for {
		select {
		case req := <-h.join:
			id := sessionCounter.Add(1)
			s := &Session{
				ID:   id,
				Nick: req.nick,
				Send: make(chan string, 64),
			}
			sessions[id] = s
			req.result <- s

			h.sendAll(sessions, nil, fmt.Sprintf("* %s joined the partyline", s.Nick))

		case s := <-h.leave:
			if _, ok := sessions[s.ID]; ok {
				delete(sessions, s.ID)
				close(s.Send)
				h.sendAll(sessions, nil, fmt.Sprintf("* %s left the partyline", s.Nick))
			}

		case msg := <-h.broadcast:
			var line string
			if msg.system {
				line = msg.text
			} else {
				line = fmt.Sprintf("[%s] %s", msg.from.Nick, msg.text)
			}
			h.sendAll(sessions, msg.from, line)

			// Forward local user messages to P2P transport
			if !msg.system && h.remoteSend != nil {
				select {
				case h.remoteSend <- RemoteMsg{Nick: msg.from.Nick, Text: msg.text}:
				default:
				}
			}

		case req := <-h.who:
			nicks := make([]string, 0, len(sessions))
			for _, s := range sessions {
				nicks = append(nicks, s.Nick)
			}
			req.result <- nicks

		case req := <-h.setNick:
			old := req.session.Nick
			req.session.Nick = req.nick
			req.result <- old
			h.sendAll(sessions, nil, fmt.Sprintf("* %s is now known as %s", old, req.nick))

		case <-h.stop:
			for _, s := range sessions {
				close(s.Send)
			}
			return
		}
	}
}

// Stop shuts down the hub.
func (h *Hub) Stop() {
	close(h.stop)
}

// Join registers a new session with the given nickname.
func (h *Hub) Join(nick string) *Session {
	result := make(chan *Session, 1)
	h.join <- joinReq{nick: nick, result: result}
	return <-result
}

// Leave removes a session from the hub.
func (h *Hub) Leave(s *Session) {
	h.leave <- s
}

// Broadcast sends a chat message from a session to all other sessions.
func (h *Hub) Broadcast(from *Session, text string) {
	h.broadcast <- bcastMsg{from: from, text: text}
}

// SystemMessage sends a system message to all sessions.
func (h *Hub) SystemMessage(text string) {
	h.broadcast <- bcastMsg{text: text, system: true}
}

// Who returns the nicknames of all connected sessions.
func (h *Hub) Who() []string {
	result := make(chan []string, 1)
	h.who <- whoReq{result: result}
	return <-result
}

// SetNick changes a session's nickname and returns the old one.
func (h *Hub) SetNick(s *Session, nick string) string {
	result := make(chan string, 1)
	h.setNick <- nickReq{session: s, nick: nick, result: result}
	return <-result
}

// SetRemoteSend sets the channel for outgoing messages to the P2P transport layer.
// Must be called before Run() or before any Broadcast calls.
func (h *Hub) SetRemoteSend(ch chan<- RemoteMsg) {
	h.remoteSend = ch
}

// DeliverRemote injects a message from a remote peer into the local partyline.
// Delivered as a system message so it is NOT re-forwarded to transport.
func (h *Hub) DeliverRemote(nick, text string) {
	h.broadcast <- bcastMsg{
		text:   fmt.Sprintf("[%s] %s", nick, text),
		system: true,
	}
}

// NodeName returns the hub's node name.
func (h *Hub) NodeName() string {
	return h.nodeName
}

func (h *Hub) sendAll(sessions map[uint64]*Session, exclude *Session, line string) {
	for _, s := range sessions {
		if exclude != nil && s.ID == exclude.ID {
			continue
		}
		select {
		case s.Send <- line:
		default:
			// drop message if session buffer is full
		}
	}
}

package proto

import (
	"testing"
)

const (
	fmtGetNodeId        = "GetNodeId: got %q"
	fmtGetEd25519Pubkey = "GetEd25519Pubkey: got %q"
	msgNilZeroValues    = "nil getters should return zero values"
)

const (
	testNodeID1     = "node-1"
	testListenAddr1 = "host-a:5000"
	testListenAddr2 = "host-b:9000"
	testPeerAddr    = "host-c:4000"
)

func TestEnvelopeGetters(t *testing.T) {
	env := &Envelope{
		MessageId:    "msg-1",
		SenderId:     "sender-1",
		LamportTs:    10,
		HopCount:     3,
		MsgType:      5,
		Payload:      []byte("data"),
		Signature:    []byte("sig"),
		SenderPubkey: []byte("pub"),
	}

	if env.GetMessageId() != "msg-1" {
		t.Errorf("GetMessageId: got %q", env.GetMessageId())
	}
	if env.GetSenderId() != "sender-1" {
		t.Errorf("GetSenderId: got %q", env.GetSenderId())
	}
	if env.GetLamportTs() != 10 {
		t.Errorf("GetLamportTs: got %d", env.GetLamportTs())
	}
	if env.GetHopCount() != 3 {
		t.Errorf("GetHopCount: got %d", env.GetHopCount())
	}
	if env.GetMsgType() != 5 {
		t.Errorf("GetMsgType: got %d", env.GetMsgType())
	}
	if string(env.GetPayload()) != "data" {
		t.Errorf("GetPayload: got %q", env.GetPayload())
	}
	if string(env.GetSignature()) != "sig" {
		t.Errorf("GetSignature: got %q", env.GetSignature())
	}
	if string(env.GetSenderPubkey()) != "pub" {
		t.Errorf("GetSenderPubkey: got %q", env.GetSenderPubkey())
	}
}

func TestEnvelopeNilGetters(t *testing.T) {
	var nilEnv *Envelope
	if nilEnv.GetMessageId() != "" {
		t.Error("nil GetMessageId should return empty")
	}
	if nilEnv.GetSenderId() != "" {
		t.Error("nil GetSenderId should return empty")
	}
	if nilEnv.GetLamportTs() != 0 {
		t.Error("nil GetLamportTs should return 0")
	}
	if nilEnv.GetHopCount() != 0 {
		t.Error("nil GetHopCount should return 0")
	}
	if nilEnv.GetMsgType() != 0 {
		t.Error("nil GetMsgType should return 0")
	}
	if nilEnv.GetPayload() != nil {
		t.Error("nil GetPayload should return nil")
	}
	if nilEnv.GetSignature() != nil {
		t.Error("nil GetSignature should return nil")
	}
	if nilEnv.GetSenderPubkey() != nil {
		t.Error("nil GetSenderPubkey should return nil")
	}
}

func TestEnvelopeResetAndString(t *testing.T) {
	env := &Envelope{MessageId: "test", SenderId: "s"}
	s := env.String()
	if s == "" {
		t.Error("String() should not be empty")
	}
	env.ProtoMessage() // just ensure it doesn't panic
	env.Reset()
	if env.GetMessageId() != "" {
		t.Error("Reset should clear fields")
	}
}

func TestEnvelopeReflectAndDescriptor(t *testing.T) {
	env := &Envelope{MessageId: "test"}
	m := env.ProtoReflect()
	if m == nil {
		t.Error("ProtoReflect should not return nil")
	}
	desc, idx := env.Descriptor()
	if len(desc) == 0 {
		t.Error("Descriptor bytes should not be empty")
	}
	if len(idx) != 1 || idx[0] != 0 {
		t.Errorf("Descriptor index: got %v", idx)
	}

	// Nil receiver
	var nilEnv *Envelope
	m2 := nilEnv.ProtoReflect()
	if m2 == nil {
		t.Error("nil ProtoReflect should not return nil")
	}
}

func TestPeerHelloGetters(t *testing.T) {
	h := &PeerHello{
		NodeId:        testNodeID1,
		Version:       "v1",
		Tags:          []string{"a", "b"},
		Reachable:     true,
		ListenAddr:    testListenAddr1,
		Ed25519Pubkey: []byte("key"),
	}

	if h.GetNodeId() != testNodeID1 {
		t.Errorf(fmtGetNodeId, h.GetNodeId())
	}
	if h.GetVersion() != "v1" {
		t.Errorf("GetVersion: got %q", h.GetVersion())
	}
	if len(h.GetTags()) != 2 {
		t.Errorf("GetTags: got %v", h.GetTags())
	}
	if !h.GetReachable() {
		t.Error("GetReachable should be true")
	}
	if h.GetListenAddr() != testListenAddr1 {
		t.Errorf("GetListenAddr: got %q", h.GetListenAddr())
	}
	if string(h.GetEd25519Pubkey()) != "key" {
		t.Errorf(fmtGetEd25519Pubkey, h.GetEd25519Pubkey())
	}
}

func TestPeerHelloNilGetters(t *testing.T) {
	var nilH *PeerHello
	if nilH.GetNodeId() != "" {
		t.Error("nil GetNodeId should return empty")
	}
	if nilH.GetVersion() != "" {
		t.Error("nil GetVersion should return empty")
	}
	if nilH.GetTags() != nil {
		t.Error("nil GetTags should return nil")
	}
	if nilH.GetReachable() {
		t.Error("nil GetReachable should return false")
	}
	if nilH.GetListenAddr() != "" {
		t.Error("nil GetListenAddr should return empty")
	}
	if nilH.GetEd25519Pubkey() != nil {
		t.Error("nil GetEd25519Pubkey should return nil")
	}
}

func TestPeerHelloResetAndMeta(t *testing.T) {
	h := &PeerHello{NodeId: testNodeID1, ListenAddr: testListenAddr1}
	s := h.String()
	if s == "" {
		t.Error("String() should not be empty")
	}
	h.ProtoMessage()
	m := h.ProtoReflect()
	if m == nil {
		t.Error("ProtoReflect should not return nil")
	}
	desc, idx := h.Descriptor()
	if len(desc) == 0 || len(idx) != 1 {
		t.Errorf("Descriptor: got %d bytes, idx=%v", len(desc), idx)
	}
	h.Reset()
	if h.GetNodeId() != "" {
		t.Error("Reset should clear fields")
	}
}

func TestPeerHelloAckGetters(t *testing.T) {
	h := &PeerHelloAck{
		NodeId:        "node-2",
		Version:       "v2",
		Tags:          []string{"x"},
		Reachable:     false,
		ListenAddr:    testListenAddr2,
		Ed25519Pubkey: []byte("key2"),
	}

	if h.GetNodeId() != "node-2" {
		t.Errorf(fmtGetNodeId, h.GetNodeId())
	}
	if h.GetVersion() != "v2" {
		t.Errorf("GetVersion: got %q", h.GetVersion())
	}
	if len(h.GetTags()) != 1 {
		t.Errorf("GetTags: got %v", h.GetTags())
	}
	if h.GetReachable() {
		t.Error("GetReachable should be false")
	}
	if h.GetListenAddr() != testListenAddr2 {
		t.Errorf("GetListenAddr: got %q", h.GetListenAddr())
	}
	if string(h.GetEd25519Pubkey()) != "key2" {
		t.Errorf(fmtGetEd25519Pubkey, h.GetEd25519Pubkey())
	}

	// Nil
	var nilH *PeerHelloAck
	if nilH.GetNodeId() != "" || nilH.GetVersion() != "" || nilH.GetTags() != nil {
		t.Error(msgNilZeroValues)
	}

	// Reset/String/Reflect/Descriptor
	_ = h.String()
	h.ProtoMessage()
	h.ProtoReflect()
	h.Descriptor()
	h.Reset()
}

func TestChatMessageGetters(t *testing.T) {
	m := &ChatMessage{Nick: "alice", Text: "hello", Timestamp: 12345}

	if m.GetNick() != "alice" {
		t.Errorf("GetNick: got %q", m.GetNick())
	}
	if m.GetText() != "hello" {
		t.Errorf("GetText: got %q", m.GetText())
	}
	if m.GetTimestamp() != 12345 {
		t.Errorf("GetTimestamp: got %d", m.GetTimestamp())
	}

	var nilM *ChatMessage
	if nilM.GetNick() != "" || nilM.GetText() != "" || nilM.GetTimestamp() != 0 {
		t.Error(msgNilZeroValues)
	}

	_ = m.String()
	m.ProtoMessage()
	m.ProtoReflect()
	m.Descriptor()
	m.Reset()
}

func TestPeerInfoGetters(t *testing.T) {
	p := &PeerInfo{
		NodeId:        "peer-1",
		Addr:          testPeerAddr,
		Reachable:     true,
		Ed25519Pubkey: []byte("pk"),
		LastSeen:      99999,
	}

	if p.GetNodeId() != "peer-1" {
		t.Errorf(fmtGetNodeId, p.GetNodeId())
	}
	if p.GetAddr() != testPeerAddr {
		t.Errorf("GetAddr: got %q", p.GetAddr())
	}
	if !p.GetReachable() {
		t.Error("GetReachable should be true")
	}
	if string(p.GetEd25519Pubkey()) != "pk" {
		t.Errorf(fmtGetEd25519Pubkey, p.GetEd25519Pubkey())
	}
	if p.GetLastSeen() != 99999 {
		t.Errorf("GetLastSeen: got %d", p.GetLastSeen())
	}

	var nilP *PeerInfo
	if nilP.GetNodeId() != "" || nilP.GetAddr() != "" || nilP.GetReachable() || nilP.GetEd25519Pubkey() != nil || nilP.GetLastSeen() != 0 {
		t.Error(msgNilZeroValues)
	}

	_ = p.String()
	p.ProtoMessage()
	p.ProtoReflect()
	p.Descriptor()
	p.Reset()
}

func TestPeerExchangeGetters(t *testing.T) {
	px := &PeerExchange{
		Peers: []*PeerInfo{{NodeId: "a"}, {NodeId: "b"}},
	}

	if len(px.GetPeers()) != 2 {
		t.Errorf("GetPeers: got %d", len(px.GetPeers()))
	}

	var nilPx *PeerExchange
	if nilPx.GetPeers() != nil {
		t.Error("nil GetPeers should return nil")
	}

	_ = px.String()
	px.ProtoMessage()
	px.ProtoReflect()
	px.Descriptor()
	px.Reset()
}

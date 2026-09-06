package webrtc

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

type Transport string

const (
	TransportTailnet Transport = "tailnet"
	// TransportRemote is retained for wire compatibility with pre-v4 clients;
	// both remote and tailnet always use relay-only ICE.
	TransportRemote Transport = "remote"
	TransportLAN    Transport = "lan"
	TransportPocket Transport = "pocket"
)

type Engine struct {
	api *pion.API
}

type Session struct {
	peerConnection *pion.PeerConnection
	outboundTrack  *pion.TrackLocalStaticRTP
	ssrc           uint32
	sequence       uint16
	timestamp      uint32
	mu             sync.Mutex
	signalMu       sync.Mutex
	onPCMUFrame    func([]byte)
	closeOnce      sync.Once
}

type Answer struct {
	SDP     string `json:"sdp"`
	Type    string `json:"type"`
	ICEMode string `json:"iceMode"`
}

func NewEngine() (*Engine, error) {
	media := &pion.MediaEngine{}
	if err := media.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		PayloadType:        0,
	}, pion.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register PCMU codec: %w", err)
	}
	return &Engine{api: pion.NewAPI(pion.WithMediaEngine(media))}, nil
}

func (e *Engine) NewSession(transport Transport, turnServers []pion.ICEServer) (*Session, error) {
	policy := pion.ICETransportPolicyAll
	if transport == TransportRemote || transport == TransportTailnet {
		policy = pion.ICETransportPolicyRelay
	}
	configuration := pion.Configuration{ICETransportPolicy: policy, ICEServers: turnServers}
	peerConnection, err := e.api.NewPeerConnection(configuration)
	if err != nil {
		return nil, fmt.Errorf("create PeerConnection: %w", err)
	}
	outboundTrack, err := pion.NewTrackLocalStaticRTP(pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "cellbridge")
	if err != nil {
		_ = peerConnection.Close()
		return nil, fmt.Errorf("create PCMU track: %w", err)
	}
	if _, err := peerConnection.AddTrack(outboundTrack); err != nil {
		_ = peerConnection.Close()
		return nil, fmt.Errorf("add PCMU track: %w", err)
	}
	var ssrcBytes [4]byte
	if _, err := rand.Read(ssrcBytes[:]); err != nil {
		_ = peerConnection.Close()
		return nil, fmt.Errorf("generate audio SSRC: %w", err)
	}
	session := &Session{peerConnection: peerConnection, outboundTrack: outboundTrack, ssrc: uint32(ssrcBytes[0])<<24 | uint32(ssrcBytes[1])<<16 | uint32(ssrcBytes[2])<<8 | uint32(ssrcBytes[3])}
	peerConnection.OnTrack(func(track *pion.TrackRemote, _ *pion.RTPReceiver) {
		go session.readRemoteTrack(track)
	})
	return session, nil
}

func (s *Session) readRemoteTrack(track *pion.TrackRemote) {
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		// Pion can surface an empty RTP packet while the peer is being
		// torn down. It is not a 20 ms PCMU frame and must not be handed to
		// the voice bridge as a real audio packet.
		if packet.PayloadType == 0 && len(packet.Payload) > 0 && s.onPCMUFrame != nil {
			s.onPCMUFrame(append([]byte(nil), packet.Payload...))
		}
	}
}

func (s *Session) OnPCMUFrame(handler func([]byte)) { s.onPCMUFrame = handler }

func (s *Session) WritePCMU(frame []byte) error {
	if len(frame) != 160 {
		return fmt.Errorf("PCMU frame has %d bytes, want 160", len(frame))
	}
	s.mu.Lock()
	packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: s.sequence, Timestamp: s.timestamp, SSRC: s.ssrc}, Payload: append([]byte(nil), frame...)}
	s.sequence++
	s.timestamp += 160
	s.mu.Unlock()
	return s.outboundTrack.WriteRTP(packet)
}

func (s *Session) CreateAnswer(ctx context.Context, offerSDP string, transport Transport) (Answer, error) {
	s.signalMu.Lock()
	defer s.signalMu.Unlock()
	if offerSDP == "" {
		return Answer{}, fmt.Errorf("empty WebRTC offer")
	}
	if s.peerConnection == nil {
		return Answer{}, fmt.Errorf("PeerConnection is closed")
	}
	if err := s.peerConnection.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: offerSDP}); err != nil {
		return Answer{}, fmt.Errorf("set remote description: %w", err)
	}
	answer, err := s.peerConnection.CreateAnswer(nil)
	if err != nil {
		return Answer{}, fmt.Errorf("create WebRTC answer: %w", err)
	}
	gatherComplete := pion.GatheringCompletePromise(s.peerConnection)
	if err := s.peerConnection.SetLocalDescription(answer); err != nil {
		return Answer{}, fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	}
	local := s.peerConnection.LocalDescription()
	if local == nil {
		return Answer{}, fmt.Errorf("missing local description")
	}
	iceMode := "all"
	if transport == TransportRemote || transport == TransportTailnet {
		iceMode = "relay"
	}
	return Answer{SDP: local.SDP, Type: "answer", ICEMode: iceMode}, nil
}

func (s *Session) PeerConnection() *pion.PeerConnection { return s.peerConnection }

func (s *Session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.peerConnection != nil {
			err = s.peerConnection.Close()
		}
	})
	return err
}

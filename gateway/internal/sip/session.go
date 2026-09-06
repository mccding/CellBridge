package sip

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
	"github.com/cellbridge/cellbridge/gateway/internal/voice"
)

type SIPCallSession struct {
	ID string
	Peer string
	Direction string // outbound/inbound
	modem *modem.ActiveCallAdapter
	audio modem.VoiceAudio
	media *MediaSession
	bridge *voice.Bridge
	ctx context.Context
	cancel context.CancelFunc
	mu sync.Mutex
	state string
}

func NewSIPCallSession(id, peer, dir string, modemCtl *modem.ActiveCallAdapter, audio modem.VoiceAudio, media *MediaSession) *SIPCallSession {
	ctx, cancel := context.WithCancel(context.Background())
	br := voice.NewBridge(audio, media)
	return &SIPCallSession{ID: id, Peer: peer, Direction: dir, modem: modemCtl, audio: audio, media: media, bridge: br, ctx: ctx, cancel: cancel, state: "init"}
}

// Dial issues the cellular dial only. It returns as soon as the modem
// accepts ATD; per the 2026-09-04 document (§20) the SIP 200 OK must wait
// for the modem call to actually answer, which AwaitBridge covers.
func (s *SIPCallSession) Dial() error {
	s.mu.Lock()
	s.state = "dialing"
	s.mu.Unlock()
	slog.Info("sip session dialing", "id", s.ID, "peer", s.Peer, "dir", s.Direction)
	if s.Direction == "outbound" {
		// Recycle the QDC507 UAC route BEFORE dialing: a route session left
		// over from a previous call keeps hw:0,4 RUNNING with a stale USB
		// stream, and any capture opened against it reads silence. The
		// fresh route must be up before ATD so the voice path is streaming
		// when the cellular call connects.
		if preparer, ok := s.audio.(interface {
			PrepareRoute(context.Context) error
		}); ok && preparer != nil {
			prepCtx, prepCancel := context.WithTimeout(s.ctx, 20*time.Second)
			if err := preparer.PrepareRoute(prepCtx); err != nil {
				prepCancel()
				return fmt.Errorf("voice route recycle failed: %w", err)
			}
			prepCancel()
		}
		// Bounded dial: an AT exchange without a deadline can wedge the
		// serial client forever (observed: a hung ATH held the port lock
		// and every later dial blocked until process restart).
		dialCtx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
		defer cancel()
		if err := s.modem.Dial(dialCtx, s.Peer); err != nil {
			return fmt.Errorf("modem dial failed: %w", err)
		}
	}
	return nil
}

// AwaitBridge waits for the cellular leg to be ANSWERED (CLCC active)
// and only then starts the PCM<->RTP bridge. Opening the UAC capture
// before the call is active wedges the ALSA ASYNC stream into an XRUN
// that reads silence for the whole call — observed on the QDC507.
func (s *SIPCallSession) AwaitBridge(ctx context.Context) error {
	answered, err := s.modem.WaitActive(ctx)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("wait cellular answer failed: %w", err)
	}
	if !answered {
		return fmt.Errorf("cellular call was not answered")
	}
	slog.Info("sip cellular answered", "id", s.ID, "peer", s.Peer)
	callID := modem.CallID(s.ID)
	// CRITICAL: the bridge must run on the SESSION context, not the
	// caller's answer-wait context. The caller cancels its ctx via
	// defer right after this returns; a readLoop bound to that ctx dies
	// immediately and the whole call is silent (observed 2026-09-06:
	// one frame at pcm_peak=111 then nothing for 15s).
	if err := s.bridge.Start(s.ctx, callID); err != nil {
		return fmt.Errorf("voice bridge failed: %w", err)
	}
	s.mu.Lock()
	s.state = "active"
	s.mu.Unlock()
	return nil
}

// Hangup tears down the bridge, closes media, and releases the modem
// line so the next call never sees "active modem call exists".
func (s *SIPCallSession) Hangup() error {
	s.cancel()
	_ = s.bridge.Stop(context.Background())
	if s.Direction == "outbound" || s.Direction == "inbound" {
		// Bounded hangup: ATH without a deadline can wedge the serial
		// client's port lock forever, blocking every later dial.
		hangupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.modem.Hangup(hangupCtx)
	}
	if s.media != nil { _ = s.media.Close() }
	s.mu.Lock()
	s.state = "ended"
	s.mu.Unlock()
	return nil
}

func (s *SIPCallSession) State() string { s.mu.Lock(); defer s.mu.Unlock(); return s.state }

package voice

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

type PCMUTransport interface {
	WritePCMU([]byte) error
	OnPCMUFrame(func([]byte))
}

// Bridge is the only place where the modem's signed PCM representation is
// converted to the V1 WebRTC PCMU wire format. It enforces 20 ms / 160-sample
// frames in both directions and keeps media work off HTTP handlers.
type Bridge struct {
	audio             modem.VoiceAudio
	media             PCMUTransport
	cellularFrame     func([]int16)
	clientFrame       func([]int16)
	stop              sync.Once
	started           bool
	callID            modem.CallID
	clientFrameSeen   bool
	cellularFrameSeen bool
	cellularFrames    uint64
	clientFrames      uint64
	cellularPeakMax   int
	clientPeakMax     int
	cellularNonZero   uint64
	clientNonZero     uint64
	cellularSum       uint64
	clientSum         uint64
	mu                sync.Mutex
}

func NewBridge(audio modem.VoiceAudio, media PCMUTransport) *Bridge {
	return &Bridge{audio: audio, media: media}
}

func (b *Bridge) SetFrameHooks(cellularFrame, clientFrame func([]int16)) {
	b.mu.Lock()
	b.cellularFrame, b.clientFrame = cellularFrame, clientFrame
	b.mu.Unlock()
}

func (b *Bridge) Start(ctx context.Context, callID modem.CallID) error {
	if b.audio == nil || b.media == nil {
		return fmt.Errorf("voice audio and WebRTC media are required")
	}
	if err := b.audio.Start(ctx, callID); err != nil {
		return err
	}
	b.mu.Lock()
	b.started = true
	b.callID = callID
	b.clientFrameSeen = false
	b.cellularFrameSeen = false
	b.mu.Unlock()
	b.media.OnPCMUFrame(func(frame []byte) {
		if len(frame) != FrameSamples {
			slog.Warn("voice client frame dropped", "call_id", callID, "samples", len(frame))
			return
		}
		pcm, err := DecodePCMU(frame)
		if err != nil {
			slog.Warn("voice client frame decode failed", "call_id", callID, "error", err)
			return
		}
		b.mu.Lock()
		if !b.started {
			b.mu.Unlock()
			return
		}
		if !b.clientFrameSeen {
			b.clientFrameSeen = true
			slog.Info("voice client audio frame received", "call_id", callID, "pcm_peak", pcmPeak(pcm))
		}
		cpeak := pcmPeak(pcm)
		b.clientFrames++
		if cpeak > b.clientPeakMax { b.clientPeakMax = cpeak }
		if cpeak > 0 { b.clientNonZero++ }
		b.clientSum += uint64(cpeak)
		if b.clientFrames%250 == 0 {
			slog.Info("voice client stats", "call_id", callID, "frames", b.clientFrames, "peak_max", b.clientPeakMax, "nonzero", b.clientNonZero, "mean", b.clientSum/uint64(b.clientFrames))
			b.clientPeakMax = 0
		}
		clientHook := b.clientFrame
		b.mu.Unlock()
		if clientHook != nil {
			clientHook(pcm)
		}
		if err := b.writePCM(pcm); err != nil {
			slog.Warn("voice modem playback failed", "call_id", callID, "error", err)
		}
	})
	go b.readLoop(ctx, callID)
	return nil
}

func (b *Bridge) readLoop(ctx context.Context, callID modem.CallID) {
	pcm := make([]int16, FrameSamples)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		count, err := b.audio.ReadPCM(pcm)
		if err != nil {
			if b.isStarted() {
				slog.Warn("voice modem capture failed", "call_id", callID, "error", err)
			}
			return
		}
		if !b.isStarted() {
			return
		}
		if count != FrameSamples {
			continue
		}
		b.mu.Lock()
		cellularHook := b.cellularFrame
		b.mu.Unlock()
		if cellularHook != nil {
			cellularHook(pcm)
		}
		b.mu.Lock()
		if !b.cellularFrameSeen {
			b.cellularFrameSeen = true
			slog.Info("voice cellular audio frame received", "call_id", callID, "pcm_peak", pcmPeak(pcm))
		}
		peak := pcmPeak(pcm)
		b.cellularFrames++
		if peak > b.cellularPeakMax { b.cellularPeakMax = peak }
		if peak > 0 { b.cellularNonZero++ }
		b.cellularSum += uint64(peak)
		if b.cellularFrames%250 == 0 {
			slog.Info("voice cellular stats", "call_id", callID, "frames", b.cellularFrames, "peak_max", b.cellularPeakMax, "nonzero", b.cellularNonZero, "mean", b.cellularSum/uint64(b.cellularFrames))
			b.cellularPeakMax = 0
		}
		b.mu.Unlock()
		frame, err := EncodePCMU(pcm)
		if err != nil {
			slog.Warn("voice cellular frame encode failed", "call_id", callID, "error", err)
			return
		}
		if err := b.media.WritePCMU(frame); err != nil {
			if b.isStarted() {
				slog.Warn("voice client playback failed", "call_id", callID, "error", err)
			}
			return
		}
	}
}

func (b *Bridge) isStarted() bool {
	b.mu.Lock()
	started := b.started
	b.mu.Unlock()
	return started
}

func (b *Bridge) writePCM(pcm []int16) error {
	// Keep the started check and the write serialized with Stop. Otherwise the
	// media callback can enter ALSA after Stop has closed the device and emit a
	// misleading "audio is not active" error during normal call teardown.
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		return nil
	}
	_, err := b.audio.WritePCM(pcm)
	return err
}

func pcmPeak(samples []int16) int {
	peak := 0
	for _, sample := range samples {
		value := int(sample)
		if value < 0 {
			value = -value
		}
		if value > peak {
			peak = value
		}
	}
	return peak
}

func (b *Bridge) Stop(ctx context.Context) error {
	b.stop.Do(func() {
		b.mu.Lock()
		started := b.started
		b.started = false
		callID := b.callID
		b.callID = ""
		b.mu.Unlock()
		if started {
			slog.Info("voice bridge stopping", "call_id", callID)
		}
		if started {
			_ = b.audio.Stop(ctx)
		}
	})
	return nil
}

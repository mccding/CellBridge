package voice

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

type fakeAudio struct {
	mu      sync.Mutex
	started bool
	frames  int
}

func (a *fakeAudio) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "fake", SampleRate: 8000, Channels: 1}, nil
}
func (a *fakeAudio) Start(context.Context, modem.CallID) error {
	a.mu.Lock()
	a.started = true
	a.mu.Unlock()
	return nil
}
func (a *fakeAudio) ReadPCM(pcm []int16) (int, error) {
	for index := range pcm {
		pcm[index] = int16(index * 10)
	}
	a.mu.Lock()
	a.frames++
	count := a.frames
	a.mu.Unlock()
	if count > 1 {
		return len(pcm), context.Canceled
	}
	return len(pcm), nil
}
func (a *fakeAudio) WritePCM(pcm []int16) (int, error) { return len(pcm), nil }
func (a *fakeAudio) Stop(context.Context) error        { return nil }
func (a *fakeAudio) Close() error                      { return nil }

type fakeMedia struct {
	mu     sync.Mutex
	writer func([]byte)
	frames [][]byte
}

func (m *fakeMedia) WritePCMU(frame []byte) error {
	m.mu.Lock()
	m.frames = append(m.frames, frame)
	m.mu.Unlock()
	return nil
}
func (m *fakeMedia) OnPCMUFrame(writer func([]byte)) { m.mu.Lock(); m.writer = writer; m.mu.Unlock() }

func TestBridgeConvertsBothDirections(t *testing.T) {
	audio := &fakeAudio{}
	media := &fakeMedia{}
	bridge := NewBridge(audio, media)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(ctx, "call-1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	media.mu.Lock()
	writer := media.writer
	frames := len(media.frames)
	media.mu.Unlock()
	if writer == nil || frames == 0 {
		t.Fatalf("media writer=%v frames=%d", writer != nil, frames)
	}
	writer(make([]byte, FrameSamples))
	if err := bridge.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

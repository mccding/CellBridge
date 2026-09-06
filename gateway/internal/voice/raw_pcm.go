package voice

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

// RawPCMAudio is the intentionally small edge adapter for a Linux PCM
// endpoint exposed as two file-like streams. It is useful for PocketBridge
// prototypes and hardware labs; a certified UAC/ALSA driver can implement the
// same modem.VoiceAudio interface without touching call/WebRTC code.
type RawPCMAudio struct {
	rx    io.Reader
	tx    io.Writer
	close func() error
	mu    sync.Mutex
	call  modem.CallID
	once  sync.Once
}

func OpenRawPCM(rxPath, txPath string) (*RawPCMAudio, error) {
	if rxPath == "" || txPath == "" {
		return nil, fmt.Errorf("raw PCM requires rx_path and tx_path")
	}
	rx, err := os.Open(rxPath)
	if err != nil {
		return nil, fmt.Errorf("open PCM RX %s: %w", rxPath, err)
	}
	tx, err := os.OpenFile(txPath, os.O_WRONLY, 0)
	if err != nil {
		_ = rx.Close()
		return nil, fmt.Errorf("open PCM TX %s: %w", txPath, err)
	}
	return NewRawPCM(rx, tx, func() error {
		rxErr := rx.Close()
		txErr := tx.Close()
		if rxErr != nil {
			return rxErr
		}
		return txErr
	}), nil
}

func NewRawPCM(rx io.Reader, tx io.Writer, closeFunc func() error) *RawPCMAudio {
	return &RawPCMAudio{rx: rx, tx: tx, close: closeFunc}
}

func (a *RawPCMAudio) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "raw-pcm", SampleRate: SampleRate, Channels: 1}, nil
}

func (a *RawPCMAudio) Start(ctx context.Context, callID modem.CallID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.call != "" && a.call != callID {
		return fmt.Errorf("PCM audio is already active")
	}
	a.call = callID
	return nil
}

func (a *RawPCMAudio) ReadPCM(samples []int16) (int, error) {
	buffer := make([]byte, len(samples)*2)
	count, err := io.ReadFull(a.rx, buffer)
	for index := 0; index+1 < count; index += 2 {
		samples[index/2] = int16(binary.LittleEndian.Uint16(buffer[index : index+2]))
	}
	return count / 2, err
}

func (a *RawPCMAudio) WritePCM(samples []int16) (int, error) {
	buffer := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(buffer[index*2:index*2+2], uint16(sample))
	}
	count, err := a.tx.Write(buffer)
	return count / 2, err
}

func (a *RawPCMAudio) Stop(context.Context) error {
	a.mu.Lock()
	a.call = ""
	a.mu.Unlock()
	return nil
}

func (a *RawPCMAudio) Close() error {
	var err error
	a.once.Do(func() {
		if a.close != nil {
			err = a.close()
		}
	})
	return err
}

var _ modem.VoiceAudio = (*RawPCMAudio)(nil)

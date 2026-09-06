package voice

import (
	"context"
	"testing"
)

func TestOpenALSARequiresBothDevices(t *testing.T) {
	if _, err := OpenALSA("", "hw:0,0"); err == nil {
		t.Fatal("empty capture device was accepted")
	}
	if _, err := OpenALSA("hw:0,0", ""); err == nil {
		t.Fatal("empty playback device was accepted")
	}
}

func TestALSAProbeRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	audio, err := OpenALSA("hw:0,0", "hw:0,0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audio.Probe(ctx); err == nil {
		t.Fatal("canceled probe succeeded")
	}
}

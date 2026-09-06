package voice

import (
	"bytes"
	"context"
	"testing"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

func TestRawPCMReadsAndWritesLittleEndianSamples(t *testing.T) {
	input := bytes.NewBuffer([]byte{0x01, 0x00, 0xff, 0x7f})
	output := &bytes.Buffer{}
	audio := NewRawPCM(input, output, nil)
	if err := audio.Start(context.Background(), modem.CallID("call-1")); err != nil {
		t.Fatal(err)
	}
	samples := make([]int16, 2)
	if count, err := audio.ReadPCM(samples); err != nil || count != 2 || samples[0] != 1 || samples[1] != 32767 {
		t.Fatalf("read = %d, %#v, %v", count, samples, err)
	}
	if count, err := audio.WritePCM([]int16{-2, 3}); err != nil || count != 2 {
		t.Fatalf("write = %d, %v", count, err)
	}
	if got, want := output.Bytes(), []byte{0xfe, 0xff, 0x03, 0x00}; !bytes.Equal(got, want) {
		t.Fatalf("PCM output = %#v, want %#v", got, want)
	}
}

package voice

import "testing"

func TestPCMUFrameRoundTrip(t *testing.T) {
	frame := make([]int16, FrameSamples)
	for index := range frame {
		frame[index] = int16(index*200 - 15000)
	}
	encoded, err := EncodePCMU(frame)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePCMU(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != FrameSamples || len(decoded) != FrameSamples {
		t.Fatalf("unexpected frame sizes: %d, %d", len(encoded), len(decoded))
	}
	for index := range frame {
		if difference := int(frame[index]) - int(decoded[index]); difference > 1024 || difference < -1024 {
			t.Fatalf("sample %d round trip error %d", index, difference)
		}
	}
}

func TestQueueDropsOldestWithoutBlocking(t *testing.T) {
	queue, err := NewQueue(2)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]int16, FrameSamples)
	second := make([]int16, FrameSamples)
	third := make([]int16, FrameSamples)
	first[0], second[0], third[0] = 1, 2, 3
	for _, frame := range [][]int16{first, second, third} {
		if err := queue.Push(frame); err != nil {
			t.Fatal(err)
		}
	}
	if queue.Len() != 2 || queue.Dropped() != 1 {
		t.Fatalf("queue len=%d dropped=%d", queue.Len(), queue.Dropped())
	}
	frame, ok := queue.Pop()
	if !ok || frame[0] != 2 {
		t.Fatalf("oldest frame was not dropped: %v", frame)
	}
}

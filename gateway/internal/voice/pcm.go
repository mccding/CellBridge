package voice

import (
	"fmt"
	"sync"
)

const (
	SampleRate   = 8000
	Channels     = 1
	FrameMillis  = 20
	FrameSamples = SampleRate * FrameMillis / 1000
)

var pcmuSegmentEndpoints = [8]int{0, 256, 512, 1024, 2048, 4096, 8192, 16384}

func ValidateFrame(frame []int16) error {
	if len(frame) != FrameSamples {
		return fmt.Errorf("PCM frame has %d samples, want %d", len(frame), FrameSamples)
	}
	return nil
}

// EncodePCMU converts signed linear PCM to G.711 μ-law, the V1 WebRTC codec.
func EncodePCMU(frame []int16) ([]byte, error) {
	if err := ValidateFrame(frame); err != nil {
		return nil, err
	}
	encoded := make([]byte, len(frame))
	for index, sample := range frame {
		encoded[index] = encodeSample(sample)
	}
	return encoded, nil
}

func DecodePCMU(encoded []byte) ([]int16, error) {
	if len(encoded) != FrameSamples {
		return nil, fmt.Errorf("PCMU frame has %d samples, want %d", len(encoded), FrameSamples)
	}
	decoded := make([]int16, len(encoded))
	for index, value := range encoded {
		decoded[index] = decodeSample(value)
	}
	return decoded, nil
}

func encodeSample(sample int16) byte {
	sign := byte(0)
	value := int(sample)
	if value < 0 {
		sign = 0x80
		value = -value
	}
	if value > 32635 {
		value = 32635
	}
	value += 132
	segment := 0
	for segment < len(pcmuSegmentEndpoints)-1 && value >= pcmuSegmentEndpoints[segment+1] {
		segment++
	}
	mantissa := (value >> (segment + 3)) & 0x0f
	return ^(sign | byte(segment<<4) | byte(mantissa))
}

func decodeSample(value byte) int16 {
	value = ^value
	sign := value & 0x80
	segment := (value >> 4) & 0x07
	mantissa := value & 0x0f
	sample := ((int(mantissa) << 3) + 132) << segment
	if sign != 0 {
		return int16(132 - sample)
	}
	return int16(sample - 132)
}

type Queue struct {
	mu       sync.Mutex
	capacity int
	frames   [][]int16
	dropped  uint64
}

func NewQueue(capacity int) (*Queue, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("voice queue capacity must be positive")
	}
	return &Queue{capacity: capacity}, nil
}

// Push copies a frame and drops the oldest frame when full. It never blocks.
func (q *Queue) Push(frame []int16) error {
	if err := ValidateFrame(frame); err != nil {
		return err
	}
	copyOfFrame := append([]int16(nil), frame...)
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.frames) == q.capacity {
		q.frames = q.frames[1:]
		q.dropped++
	}
	q.frames = append(q.frames, copyOfFrame)
	return nil
}

func (q *Queue) Pop() ([]int16, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.frames) == 0 {
		return nil, false
	}
	frame := q.frames[0]
	q.frames[0] = nil
	q.frames = q.frames[1:]
	return frame, true
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.frames)
}

func (q *Queue) Dropped() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

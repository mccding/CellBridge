package eventbus

import (
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/id"
)

type Event struct {
	ID        string `json:"id"`
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	CreatedAt int64  `json:"createdAt"`
	Data      any    `json:"data"`
}

type Bus struct {
	mu          sync.RWMutex
	subscribers map[int]chan Event
	nextID      int
	now         func() time.Time
}

func New() *Bus {
	return &Bus{subscribers: make(map[int]chan Event), now: time.Now}
}

func (b *Bus) Publish(seq int64, eventType string, data any) {
	eventID, err := id.New("evt_", 16)
	if err != nil {
		return
	}
	event := Event{ID: eventID, Seq: seq, Type: eventType, CreatedAt: b.now().Unix(), Data: data}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, subscriber := range b.subscribers {
		select {
		case subscriber <- event:
		default:
			// A slow UI subscriber must not block modem or HTTP workers.
		}
	}
}

func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	key := b.nextID
	channel := make(chan Event, 32)
	b.subscribers[key] = channel
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subscribers, key)
			close(channel)
			b.mu.Unlock()
		})
	}
	return channel, cancel
}

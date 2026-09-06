package events

import (
	"sync"

	"github.com/cellbridge/cellbridge/module-agent/internal/model"
)

type Journal struct {
	mu        sync.Mutex
	seq       int64
	entries   []model.Event
	limit     int
	listeners map[chan model.Event]struct{}
}

func NewJournal(limit int) *Journal {
	if limit < 1 {
		limit = 128
	}
	return &Journal{limit: limit, listeners: make(map[chan model.Event]struct{})}
}

func (j *Journal) Append(kind string, payload interface{}, unix int64) model.Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	event := model.Event{Seq: j.seq, Type: kind, Time: unix, Payload: payload}
	j.entries = append(j.entries, event)
	if len(j.entries) > j.limit {
		j.entries = j.entries[len(j.entries)-j.limit:]
	}
	for listener := range j.listeners {
		select {
		case listener <- event:
		default:
		}
	}
	return event
}

func (j *Journal) Since(seq int64) ([]model.Event, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.entries) == 0 {
		return nil, true
	}
	if seq < j.entries[0].Seq-1 {
		return nil, false
	}
	result := make([]model.Event, 0, len(j.entries))
	for _, event := range j.entries {
		if event.Seq > seq {
			result = append(result, event)
		}
	}
	return result, true
}

func (j *Journal) Subscribe() (<-chan model.Event, func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	channel := make(chan model.Event, 16)
	j.listeners[channel] = struct{}{}
	return channel, func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		if _, ok := j.listeners[channel]; ok {
			delete(j.listeners, channel)
			close(channel)
		}
	}
}

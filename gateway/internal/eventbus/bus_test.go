package eventbus

import (
	"testing"
	"time"
)

func TestPublishIsNonBlockingForSlowSubscribers(t *testing.T) {
	bus := New()
	bus.now = func() time.Time { return time.Unix(42, 0) }
	_, cancel := bus.Subscribe()
	defer cancel()
	for index := 0; index < 100; index++ {
		bus.Publish(int64(index+1), "line.updated", map[string]string{"status": "ready"})
	}
}

func TestSubscriberReceivesEventAndCancelIsIdempotent(t *testing.T) {
	bus := New()
	events, cancel := bus.Subscribe()
	bus.Publish(4, "message.created", map[string]string{"id": "msg-1"})
	event := <-events
	if event.Seq != 4 || event.Type != "message.created" || event.ID == "" {
		t.Fatalf("event = %#v", event)
	}
	cancel()
	cancel()
}

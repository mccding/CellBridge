package modem

import (
	"context"
	"sync"
)

// ActiveCallAdapter translates the modem's opaque CallID into the single
// active-call control surface used by the V1 HTTP state machine. V1 permits
// one SIM/module and one concurrent call, so this mapping is intentionally
// small and is reset on hangup.
type ActiveCallAdapter struct {
	Control      ModemControl
	mu           sync.Mutex
	active       CallID
	logical      CallID
	lastPhysical CallID
	lastLogical  CallID
	events       chan ModemEvent
	close        sync.Once
}

func NewActiveCallAdapter(control ModemControl) *ActiveCallAdapter {
	adapter := &ActiveCallAdapter{Control: control, events: make(chan ModemEvent, 32)}
	go adapter.forwardEvents()
	return adapter
}

func (a *ActiveCallAdapter) SetLogicalCallID(callID string) {
	a.mu.Lock()
	a.logical = CallID(callID)
	a.mu.Unlock()
}

func (a *ActiveCallAdapter) Dial(ctx context.Context, peer string) error {
	callID, err := a.Control.Dial(ctx, peer)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.active = callID
	a.lastPhysical = ""
	a.lastLogical = ""
	a.mu.Unlock()
	return nil
}

func (a *ActiveCallAdapter) Answer(ctx context.Context) error {
	return a.withActive(func(callID CallID) error { return a.Control.Answer(ctx, callID) })
}

func (a *ActiveCallAdapter) Hangup(ctx context.Context) error {
	err := a.withActive(func(callID CallID) error { return a.Control.Hangup(ctx, callID) })
	if err == nil {
		a.mu.Lock()
		a.lastPhysical = a.active
		a.lastLogical = a.logical
		a.active = ""
		a.logical = ""
		a.mu.Unlock()
	}
	return err
}

func (a *ActiveCallAdapter) DTMF(ctx context.Context, digit rune) error {
	return a.withActive(func(callID CallID) error { return a.Control.SendDTMF(ctx, callID, digit) })
}

// WaitActive blocks until the outgoing cellular call is answered. It
// forwards to the AT adapter's CLCC poll.
func (a *ActiveCallAdapter) WaitActive(ctx context.Context) (bool, error) {
	if waiter, ok := a.Control.(interface {
		WaitActive(context.Context) (bool, error)
	}); ok {
		return waiter.WaitActive(ctx)
	}
	// Backend without CLCC support: treat as immediately answered.
	return true, nil
}

func (a *ActiveCallAdapter) withActive(action func(CallID) error) error {
	a.mu.Lock()
	callID := a.active
	a.mu.Unlock()
	if callID == "" {
		return ErrNoActiveCall
	}
	return action(callID)
}

func (a *ActiveCallAdapter) Events() <-chan ModemEvent { return a.events }

func (a *ActiveCallAdapter) Close() error {
	var err error
	a.close.Do(func() { err = a.Control.Close() })
	return err
}

func (a *ActiveCallAdapter) forwardEvents() {
	defer close(a.events)
	for event := range a.Control.Events() {
		a.mu.Lock()
		physicalID := event.CallID
		logicalID := a.logical
		if physicalID == a.lastPhysical && a.lastLogical != "" {
			logicalID = a.lastLogical
		}
		if event.Kind == "incoming" && physicalID != "" && a.active == "" {
			a.active = physicalID
		}
		if physicalID != "" && (physicalID == a.active || physicalID == a.lastPhysical) && logicalID != "" {
			event.CallID = logicalID
		}
		if event.Kind == "ended" && (physicalID == a.active || physicalID == a.lastPhysical) {
			if a.lastPhysical == "" {
				a.lastPhysical = physicalID
				a.lastLogical = a.logical
			}
			a.active = ""
			a.logical = ""
		}
		a.mu.Unlock()
		select {
		case a.events <- event:
		default:
		}
	}
}

var ErrNoActiveCall = errorString("no active modem call")

type errorString string

func (e errorString) Error() string { return string(e) }

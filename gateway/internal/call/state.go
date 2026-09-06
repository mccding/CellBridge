package call

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Direction string

const (
	Inbound  Direction = "inbound"
	Outbound Direction = "outbound"
)

type State string

const (
	Idle            State = "idle"
	IncomingRinging State = "incoming_ringing"
	OutgoingDialing State = "outgoing_dialing"
	Connecting      State = "connecting"
	Active          State = "active"
	Ending          State = "ending"
	Recovering      State = "recovering"
)

var (
	ErrLineBusy       = errors.New("cellular line is busy")
	ErrCallNotFound   = errors.New("call not found")
	ErrInvalidState   = errors.New("invalid call state transition")
	ErrDuplicateEvent = errors.New("duplicate modem event")
	ErrInvalidDTMF    = errors.New("invalid DTMF digit")
	ErrEmergencyCall  = errors.New("emergency numbers must be dialed with the system phone")
)

var emergencyNumbers = map[string]struct{}{
	"112": {}, "911": {}, "999": {}, "110": {}, "119": {}, "120": {}, "122": {},
}

// IsEmergencyNumber intentionally matches only the normalized dial string.
// CellBridge cannot provide the reliability guarantees required for emergency
// calling, so the API must direct these calls to the iOS system Phone app.
func IsEmergencyNumber(value string) bool {
	var digits strings.Builder
	for _, character := range value {
		if character >= '0' && character <= '9' {
			digits.WriteRune(character)
		}
	}
	normalized := digits.String()
	if strings.HasPrefix(strings.TrimSpace(value), "+86") && strings.HasPrefix(normalized, "86") {
		normalized = strings.TrimPrefix(normalized, "86")
	}
	_, blocked := emergencyNumbers[normalized]
	return blocked
}

type Call struct {
	ID            string     `json:"id"`
	Direction     Direction  `json:"direction"`
	Peer          string     `json:"peer"`
	State         State      `json:"state"`
	StartedAt     time.Time  `json:"startedAt"`
	ConnectedAt   *time.Time `json:"connectedAt,omitempty"`
	EndedAt       *time.Time `json:"endedAt,omitempty"`
	EndReason     string     `json:"endReason,omitempty"`
	CellularReady bool       `json:"-"`
	MediaReady    bool       `json:"-"`
	Answered      bool       `json:"-"`
}

type Event struct {
	CallID string
	State  State
	Kind   string
	Reason string
}

type Controller struct {
	mu       sync.Mutex
	active   *Call
	requests map[string]result
	now      func() time.Time
}

type result struct {
	call     Call
	event    Event
	err      error
	previous *Call
}

func NewController() *Controller {
	return &Controller{requests: make(map[string]result), now: time.Now}
}

func (c *Controller) Snapshot() *Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return nil
	}
	copy := *c.active
	return &copy
}

func (c *Controller) Incoming(callID, peer string) (Call, Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		if c.active.ID == callID {
			return *c.active, Event{CallID: callID, State: c.active.State, Kind: "duplicate_incoming"}, ErrDuplicateEvent
		}
		return Call{}, Event{}, ErrLineBusy
	}
	call := Call{ID: callID, Direction: Inbound, Peer: peer, State: IncomingRinging, StartedAt: c.now()}
	c.active = &call
	event := Event{CallID: callID, State: IncomingRinging, Kind: "call.incoming"}
	return call, event, nil
}

func (c *Controller) Dial(requestID, callID, peer string) (Call, Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous, ok := c.requests[requestID]; ok {
		return previous.call, previous.event, previous.err
	}
	if c.active != nil {
		return Call{}, Event{}, ErrLineBusy
	}
	call := Call{ID: callID, Direction: Outbound, Peer: peer, State: OutgoingDialing, StartedAt: c.now()}
	event := Event{CallID: callID, State: OutgoingDialing, Kind: "call.updated"}
	c.active = &call
	c.requests[requestID] = result{call: call, event: event}
	return call, event, nil
}

func (c *Controller) Answer(requestID, callID string) (Call, Event, error) {
	return c.mutate(requestID, callID, "answer", func(call *Call) error {
		if call.State != IncomingRinging {
			return fmt.Errorf("%w: answer from %s", ErrInvalidState, call.State)
		}
		call.Answered = true
		call.State = Connecting
		return nil
	})
}

func (c *Controller) MarkCellularReady(callID string) (Call, Event, error) {
	return c.mutate("cellular-ready:"+callID, callID, "cellular_ready", func(call *Call) error {
		if call.State != OutgoingDialing && call.State != Connecting {
			return fmt.Errorf("%w: cellular ready from %s", ErrInvalidState, call.State)
		}
		call.CellularReady = true
		if call.MediaReady {
			call.State = Active
			if call.ConnectedAt == nil {
				connected := c.now()
				call.ConnectedAt = &connected
			}
		} else {
			call.State = Connecting
		}
		return nil
	})
}

func (c *Controller) MarkMediaReady(callID string) (Call, Event, error) {
	return c.mutate("media-ready:"+callID, callID, "media_ready", func(call *Call) error {
		if call.State != OutgoingDialing && call.State != Connecting && call.State != Active && call.State != Recovering {
			return fmt.Errorf("%w: media ready from %s", ErrInvalidState, call.State)
		}
		call.MediaReady = true
		if call.CellularReady {
			call.State = Active
			if call.ConnectedAt == nil {
				connected := c.now()
				call.ConnectedAt = &connected
			}
		} else {
			call.State = Connecting
		}
		return nil
	})
}

func (c *Controller) MarkRecovering(callID, reason string) (Call, Event, error) {
	return c.mutate("recovering:"+callID+":"+reason, callID, "call.recovering", func(call *Call) error {
		if call.State != OutgoingDialing && call.State != Connecting && call.State != Active {
			return fmt.Errorf("%w: recovering from %s", ErrInvalidState, call.State)
		}
		call.State = Recovering
		call.MediaReady = false
		call.EndReason = reason
		return nil
	})
}

func (c *Controller) Hangup(requestID, callID, reason string) (Call, Event, error) {
	return c.mutate(requestID, callID, "hangup", func(call *Call) error {
		if call.State == Ending {
			return nil
		}
		if call.State == Idle {
			return fmt.Errorf("%w: hangup from idle", ErrInvalidState)
		}
		call.State = Ending
		call.EndReason = reason
		return nil
	})
}

// Rollback restores the call state changed by a request whose modem command
// failed. The request result is removed so a later retry can use a new
// idempotency key without inheriting a stale in-memory transition.
func (c *Controller) Rollback(requestID, callID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, ok := c.requests[requestID]
	if !ok || previous.previous == nil || c.active == nil || c.active.ID != callID {
		return ErrCallNotFound
	}
	restored := *previous.previous
	c.active = &restored
	delete(c.requests, requestID)
	return nil
}

func (c *Controller) RemoteEnded(callID, reason string) (Call, Event, error) {
	return c.mutate("remote-ended:"+callID+":"+reason, callID, "call.ended", func(call *Call) error {
		if call.State == Ending {
			return nil
		}
		call.State = Ending
		call.EndReason = reason
		return nil
	})
}

func (c *Controller) DTMF(callID string, digit rune) error {
	if !strings.ContainsRune("0123456789*#ABCD", digit) {
		return ErrInvalidDTMF
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || c.active.ID != callID {
		return ErrCallNotFound
	}
	if c.active.State != Active {
		return fmt.Errorf("%w: DTMF from %s", ErrInvalidState, c.active.State)
	}
	return nil
}

func (c *Controller) Finish(callID string) (Call, Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || c.active.ID != callID {
		return Call{}, Event{}, ErrCallNotFound
	}
	if c.active.State != Ending {
		return Call{}, Event{}, fmt.Errorf("%w: finish from %s", ErrInvalidState, c.active.State)
	}
	ended := c.now()
	c.active.EndedAt = &ended
	call := *c.active
	event := Event{CallID: callID, State: Ending, Kind: "call.ended", Reason: call.EndReason}
	c.active = nil
	return call, event, nil
}

func (c *Controller) mutate(requestID, callID, kind string, change func(*Call) error) (Call, Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous, ok := c.requests[requestID]; ok {
		return previous.call, previous.event, previous.err
	}
	if c.active == nil || c.active.ID != callID {
		return Call{}, Event{}, ErrCallNotFound
	}
	previousCall := *c.active
	if err := change(c.active); err != nil {
		return Call{}, Event{}, err
	}
	event := Event{CallID: callID, State: c.active.State, Kind: kind}
	if c.active.State == Ending {
		event.Reason = c.active.EndReason
	}
	call := *c.active
	c.requests[requestID] = result{call: call, event: event, previous: &previousCall}
	return call, event, nil
}

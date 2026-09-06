package modem

import (
	"context"
	"testing"
	"time"
)

type fakeCallControl struct {
	events chan ModemEvent
	last   CallID
}

func (f *fakeCallControl) Probe(context.Context) (Capabilities, error) { return Capabilities{}, nil }
func (f *fakeCallControl) Status(context.Context) (LineStatus, error)  { return LineStatus{}, nil }
func (f *fakeCallControl) Dial(context.Context, string) (CallID, error) {
	f.last = "physical-1"
	return f.last, nil
}
func (f *fakeCallControl) Answer(context.Context, CallID) error { return nil }
func (f *fakeCallControl) Hangup(context.Context, CallID) error {
	f.events <- ModemEvent{Kind: "ended", CallID: f.last}
	return nil
}
func (f *fakeCallControl) SendDTMF(context.Context, CallID, rune) error { return nil }
func (f *fakeCallControl) ListSMS(context.Context, SMSCursor) ([]RawSMS, SMSCursor, error) {
	return nil, "", nil
}
func (f *fakeCallControl) SendSMS(context.Context, string, SMSPayload) (SMSID, error) {
	return "", nil
}
func (f *fakeCallControl) DeleteSMS(context.Context, string) error { return nil }
func (f *fakeCallControl) Events() <-chan ModemEvent               { return f.events }
func (f *fakeCallControl) Close() error                            { close(f.events); return nil }

func TestActiveCallAdapterMapsIncomingAndEndedHandles(t *testing.T) {
	control := &fakeCallControl{events: make(chan ModemEvent, 4)}
	adapter := NewActiveCallAdapter(control)
	defer adapter.Close()
	control.events <- ModemEvent{Kind: "incoming", CallID: "physical-incoming"}
	select {
	case event := <-adapter.Events():
		if event.CallID != "physical-incoming" {
			t.Fatalf("incoming call id = %q", event.CallID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for incoming event")
	}
	control.last = "physical-incoming"
	if err := adapter.Answer(context.Background()); err != nil {
		t.Fatalf("answer incoming call: %v", err)
	}
	adapter.SetLogicalCallID("call-logical")
	if err := adapter.Hangup(context.Background()); err != nil {
		t.Fatalf("hangup incoming call: %v", err)
	}
	select {
	case event := <-adapter.Events():
		if event.CallID != "call-logical" {
			t.Fatalf("ended call id = %q", event.CallID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ended event")
	}
}

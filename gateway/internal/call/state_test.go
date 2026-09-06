package call

import (
	"errors"
	"testing"
	"time"
)

func TestIncomingAnswerRequiresBothReadinessSignals(t *testing.T) {
	controller := NewController()
	controller.now = func() time.Time { return time.Unix(100, 0) }
	if _, event, err := controller.Incoming("call-1", "+86138"); err != nil || event.Kind != "call.incoming" {
		t.Fatalf("incoming = %v, %v", event, err)
	}
	if _, _, err := controller.Answer("req-answer", "call-1"); err != nil {
		t.Fatal(err)
	}
	call, event, err := controller.MarkMediaReady("call-1")
	if err != nil || call.State != Connecting || event.State != Connecting {
		t.Fatalf("media ready = %#v, %v, %v", call, event, err)
	}
	call, event, err = controller.MarkCellularReady("call-1")
	if err != nil || call.State != Active || call.ConnectedAt == nil || event.State != Active {
		t.Fatalf("cellular ready = %#v, %v, %v", call, event, err)
	}
}

func TestDuplicateRingAndSingleLine(t *testing.T) {
	controller := NewController()
	if _, _, err := controller.Incoming("call-1", "+86138"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.Incoming("call-1", "+86138"); !errors.Is(err, ErrDuplicateEvent) {
		t.Fatalf("duplicate ring error = %v", err)
	}
	if _, _, err := controller.Incoming("call-2", "+86139"); !errors.Is(err, ErrLineBusy) {
		t.Fatalf("second call error = %v", err)
	}
}

func TestIdempotentDial(t *testing.T) {
	controller := NewController()
	first, _, err := controller.Dial("request-1", "call-1", "10086")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := controller.Dial("request-1", "call-other", "10010")
	if err != nil || second.ID != first.ID {
		t.Fatalf("idempotent dial = %#v, %v", second, err)
	}
}

func TestRemoteHangupFinishesAndReleasesLine(t *testing.T) {
	controller := NewController()
	if _, _, err := controller.Dial("request-1", "call-1", "10086"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.RemoteEnded("call-1", "remote_ended"); err != nil {
		t.Fatal(err)
	}
	if _, event, err := controller.Finish("call-1"); err != nil || event.Kind != "call.ended" {
		t.Fatalf("finish = %v, %v", event, err)
	}
	if controller.Snapshot() != nil {
		t.Fatal("line remains occupied after finish")
	}
}

func TestMediaRecoveryCanReturnToActive(t *testing.T) {
	controller := NewController()
	if _, _, err := controller.Dial("request-1", "call-1", "10086"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.MarkCellularReady("call-1"); err != nil {
		t.Fatal(err)
	}
	if _, event, err := controller.MarkMediaReady("call-1"); err != nil || event.State != Active {
		t.Fatalf("active = %v, %v", event, err)
	}
	if _, event, err := controller.MarkRecovering("call-1", "ice_failed"); err != nil || event.State != Recovering {
		t.Fatalf("recovering = %v, %v", event, err)
	}
	call, event, err := controller.MarkMediaReady("call-1")
	if err != nil || call.State != Active || event.State != Active {
		t.Fatalf("recovered = %#v, %v, %v", call, event, err)
	}
}

func TestOutgoingCallEntersConnectingBeforeMediaIsReady(t *testing.T) {
	controller := NewController()
	if _, _, err := controller.Dial("request-1", "call-1", "10086"); err != nil {
		t.Fatal(err)
	}
	current, event, err := controller.MarkCellularReady("call-1")
	if err != nil || current.State != Connecting || event.State != Connecting {
		t.Fatalf("cellular ready = %#v, %v, %v", current, event, err)
	}
	current, event, err = controller.MarkMediaReady("call-1")
	if err != nil || current.State != Active || event.State != Active {
		t.Fatalf("media ready = %#v, %v, %v", current, event, err)
	}
}

func TestEmergencyNumberDetection(t *testing.T) {
	for _, value := range []string{"112", "911", "999", "110", "119", "120", "122", "+86 110"} {
		if !IsEmergencyNumber(value) {
			t.Fatalf("IsEmergencyNumber(%q) = false", value)
		}
	}
	if IsEmergencyNumber("10086") {
		t.Fatal("ordinary service number was blocked")
	}
}

func TestRollbackRestoresStateAfterExternalCommandFailure(t *testing.T) {
	controller := NewController()
	if _, _, err := controller.Incoming("call-1", "+86138"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.Answer("answer-1", "call-1"); err != nil {
		t.Fatal(err)
	}
	if err := controller.Rollback("answer-1", "call-1"); err != nil {
		t.Fatal(err)
	}
	if snapshot := controller.Snapshot(); snapshot == nil || snapshot.State != IncomingRinging {
		t.Fatalf("snapshot after rollback = %#v", snapshot)
	}
	if _, _, err := controller.Answer("answer-2", "call-1"); err != nil {
		t.Fatalf("answer after rollback = %v", err)
	}
}

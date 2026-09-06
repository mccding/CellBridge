package qdc507

import "testing"

func TestSelectTransportIDPinsTheExactUSBPath(t *testing.T) {
	devices := "List of devices attached\n(serialless)\tdevice usb:2-1 transport_id:2\n(other)\tdevice usb:3-2 transport_id:3\n"
	if got, err := SelectTransportID(devices, "2-1"); err != nil || got != "2" {
		t.Fatalf("got transport=%q err=%v", got, err)
	}
	if _, err := SelectTransportID(devices, "1-1"); err == nil {
		t.Fatal("unrelated USB path was accepted")
	}
	if _, err := SelectTransportID(devices+"duplicate\tdevice usb:2-1 transport_id:4\n", "2-1"); err == nil {
		t.Fatal("ambiguous USB path was accepted")
	}
}

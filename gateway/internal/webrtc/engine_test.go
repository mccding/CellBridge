package webrtc

import (
	"context"
	"testing"
)

func TestV1RegistersPCMUAndRemoteUsesRelay(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.NewSession(TransportRemote, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.PeerConnection().GetConfiguration().ICETransportPolicy; got != 1 { // Relay is Pion's enum value 1.
		t.Fatalf("ICE policy = %v, want relay", got)
	}
	if _, err := session.CreateAnswer(context.Background(), "", TransportRemote); err == nil {
		t.Fatal("empty offer was accepted")
	}
}

func TestPocketAndLANAllowDirectICE(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []Transport{TransportLAN, TransportPocket} {
		session, err := engine.NewSession(transport, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := session.PeerConnection().GetConfiguration().ICETransportPolicy; got != 0 {
			t.Fatalf("%s ICE policy = %v, want all", transport, got)
		}
		_ = session.Close()
	}
}

func TestPCMUTrackAcceptsExactlyOneTelephoneFrame(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.NewSession(TransportLAN, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.WritePCMU(make([]byte, 160)); err != nil {
		t.Fatal(err)
	}
	if err := session.WritePCMU(make([]byte, 159)); err == nil {
		t.Fatal("short PCMU frame was accepted")
	}
}

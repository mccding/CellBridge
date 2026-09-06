package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPlanConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("server:\n  listen: 127.0.0.1:9090\ndata:\n  dir: /tmp/cellbridge\nvoice:\n  enabled: true\n  codec: pcmu\n  sample_rate: 8000\nsecurity:\n  pairing_local_only: true\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Server.Listen != "127.0.0.1:9090" || loaded.Voice.Codec != "pcmu" || loaded.WebRTC.RemoteICEPolicy != "relay" {
		t.Fatalf("loaded = %#v", loaded)
	}
}

func TestRejectsNonV1VoiceConfiguration(t *testing.T) {
	value := Default()
	value.Voice.Codec = "opus"
	if err := value.Validate(); err == nil {
		t.Fatal("opus was accepted for V1")
	}
}

func TestRecordingAllowsManualModeWithAutoRecordingOff(t *testing.T) {
	value := Default()
	value.Recording.Enabled = true
	value.Recording.AutoMode = "off"
	if err := value.Validate(); err != nil {
		t.Fatalf("manual recording configuration rejected: %v", err)
	}
}

func TestQDC507VoiceBackendRequiresExternalRuntime(t *testing.T) {
	value := Default()
	value.Voice.Enabled = true
	value.Voice.Backend = "qdc507"
	value.Voice.RXPath = "hw:0,0"
	value.Voice.TXPath = "hw:0,0"
	if err := value.Validate(); err == nil {
		t.Fatal("QDC507 backend accepted without external runtime")
	}
	value.Voice.RuntimeDir = "/opt/cellbridge/module-voice"
	if err := value.Validate(); err != nil {
		t.Fatalf("QDC507 backend rejected with runtime directory: %v", err)
	}
	if value.Voice.Bootstrap {
		t.Fatal("QDC507 bootstrap must default to disabled")
	}
}

func TestTailnetModeRequiresLoopbackAndDisablesPublicFallback(t *testing.T) {
	value := Default()
	value.Network.TailnetHostname = "cellbridge-nas.example.ts.net"
	if err := value.Validate(); err != nil {
		t.Fatalf("valid tailnet configuration rejected: %v", err)
	}

	value.Server.Listen = "0.0.0.0:8787"
	if err := value.Validate(); err == nil {
		t.Fatal("tailnet configuration accepted a non-loopback bind")
	}

	value = Default()
	value.Network.PublicFallback = true
	if err := value.Validate(); err == nil {
		t.Fatal("public fallback was accepted")
	}
}

func TestPrivateTURNRequiresExplicitHost(t *testing.T) {
	value := Default()
	value.WebRTC.PrivateTURN.Enabled = true
	if err := value.Validate(); err == nil {
		t.Fatal("private TURN was accepted without a host")
	}
	value.WebRTC.PrivateTURN.Host = "cellbridge-nas.example.ts.net"
	if err := value.Validate(); err != nil {
		t.Fatalf("private TURN configuration rejected: %v", err)
	}
}

package recording

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/db"
)

type testEncoder struct{}

func (testEncoder) Encode(_ context.Context, cellular, client, output string) error {
	cellularBytes, err := os.ReadFile(cellular)
	if err != nil {
		return err
	}
	clientBytes, err := os.ReadFile(client)
	if err != nil {
		return err
	}
	return os.WriteFile(output, append(cellularBytes, clientBytes...), 0o640)
}

func TestManagerCapturesFinalizesAndDeletesRecording(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Second)
	if _, err := database.SaveCall(context.Background(), db.Call{ID: "call-1", Direction: "outbound", Peer: "+86138", State: "active", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManagerWithEncoder(database, filepath.Join(t.TempDir(), "recordings"), 90, testEncoder{})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetMinimumFreeBytes(0)
	var events []string
	manager.SetEventHandler(func(kind string, _ db.Recording) { events = append(events, kind) })
	recording, err := manager.Start(context.Background(), CallMeta{ID: "call-1", Direction: "outbound", Peer: "+86138", StartedAt: started}, "manual", "device-1")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := manager.Start(context.Background(), CallMeta{ID: "call-1", StartedAt: started}, "manual", "device-1")
	if err != nil || retry.ID != recording.ID {
		t.Fatalf("idempotent start = %#v, %v", retry, err)
	}
	if len(events) < 2 || events[0] != "recording.starting" || events[1] != "recording.started" {
		t.Fatalf("start events = %#v", events)
	}
	if err := manager.WriteCellularFrame("call-1", make([]int16, 160)); err != nil {
		t.Fatal(err)
	}
	if err := manager.WriteClientFrame("call-1", []int16{1000, -1000, 500, -500}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), "call-1", "hangup"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var ready db.Recording
	for time.Now().Before(deadline) {
		ready, err = manager.Get(context.Background(), recording.ID)
		if err == nil && ready.State == "ready" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ready.State != "ready" || ready.DurationMs == nil || *ready.DurationMs != 20 {
		t.Fatalf("final recording = %#v", ready)
	}
	if _, err := os.Stat(ready.FilePath); err != nil {
		t.Fatalf("final audio = %v", err)
	}
	if _, err := os.Stat(ready.WaveformPath); err != nil {
		t.Fatalf("waveform = %v", err)
	}
	second, err := manager.Start(context.Background(), CallMeta{ID: "call-1", Direction: "outbound", Peer: "+86138", StartedAt: started}, "manual", "device-1")
	if err != nil {
		t.Fatalf("start second segment: %v", err)
	}
	if second.ID == ready.ID {
		t.Fatalf("second segment reused first recording: %#v", second)
	}
	if err := manager.Stop(context.Background(), "call-1", "hangup"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	var secondReady db.Recording
	for time.Now().Before(deadline) {
		secondReady, err = manager.Get(context.Background(), second.ID)
		if err == nil && secondReady.State == "ready" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if secondReady.State != "ready" {
		t.Fatalf("second recording = %#v", secondReady)
	}
	if err := manager.Delete(context.Background(), ready.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := manager.Get(context.Background(), ready.ID)
	if err != nil || deleted.State != "deleted" {
		t.Fatalf("deleted recording = %#v, %v", deleted, err)
	}
	if _, err := os.Stat(ready.FilePath); !os.IsNotExist(err) {
		t.Fatalf("audio still exists, stat error = %v", err)
	}
	if err := manager.Delete(context.Background(), secondReady.ID); err != nil {
		t.Fatal(err)
	}
}

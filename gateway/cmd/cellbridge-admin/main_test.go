package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cellbridge/cellbridge/gateway/internal/db"
)

func TestBackupAndRestoreKeepsDatabaseConsistent(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Open(filepath.Join(dataDir, "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "cellbridge-backup.zip")
	if err := backup(context.Background(), []string{"--data-dir", dataDir, "--output", archive}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	}
	if err := restore([]string{"--data-dir", dataDir, "--input", archive, "--confirm-restore"}); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(filepath.Join(dataDir, "cellbridge.sqlite.pre-restore-*"))
	if err != nil || len(backups) != 1 {
		t.Fatal("restore backup should have a timestamped name")
	}
}

func TestBackupAndRestoreIncludesRecordingsWhenRequested(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Open(filepath.Join(dataDir, "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	recordingPath := filepath.Join(dataDir, "recordings", "2026", "09", "example.m4a")
	if err := os.MkdirAll(filepath.Dir(recordingPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordingPath, []byte("recording"), 0o640); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "cellbridge-recordings-backup.zip")
	if err := backup(context.Background(), []string{"--data-dir", dataDir, "--output", archive, "--include-recordings"}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dataDir, "recordings")); err != nil {
		t.Fatal(err)
	}
	if err := restore([]string{"--data-dir", dataDir, "--input", archive, "--confirm-restore"}); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(recordingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != "recording" {
		t.Fatalf("restored recording = %q", restored)
	}
}

func TestArchivePathRejectsTraversal(t *testing.T) {
	for _, path := range []string{"../db.sqlite", "/tmp/db.sqlite", "recordings/../db.sqlite"} {
		if allowedArchivePath(path) {
			t.Fatalf("allowed unsafe archive path %q", path)
		}
	}
	if !allowedArchivePath("db.sqlite") || !allowedArchivePath("recordings/example.wav") {
		t.Fatal("rejected expected safe archive path")
	}
}

func TestRevokeDeviceRequiresConfirmationAndInvalidatesDevice(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Open(filepath.Join(dataDir, "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateDevice(context.Background(), "dev_test", "Test iPhone", []byte("public-key")); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := revokeDevice(context.Background(), []string{"--data-dir", dataDir, "--device-id", "dev_test"}); err == nil {
		t.Fatal("revoke should require confirmation")
	}
	if err := revokeDevice(context.Background(), []string{"--data-dir", dataDir, "--device-id", "dev_test", "--confirm-revoke"}); err != nil {
		t.Fatal(err)
	}
	check, err := db.Open(filepath.Join(dataDir, "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var revokedAt int64
	if err := check.QueryRowContext(context.Background(), "SELECT revoked_at FROM devices WHERE id = ?", "dev_test").Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt == 0 {
		t.Fatal("device was not revoked")
	}
}

func TestRevokeMissingDeviceFails(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Open(filepath.Join(dataDir, "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := revokeDevice(context.Background(), []string{"--data-dir", dataDir, "--device-id", "dev_missing", "--confirm-revoke"}); err == nil {
		t.Fatal("revoking a missing device should fail")
	}
}

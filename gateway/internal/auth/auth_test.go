package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/db"
)

func TestPairingIssuesAndRevokesCredentials(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000, 0)
	service := NewService("gw_test", database)
	service.Now = func() time.Time { return now }
	started, err := service.StartPairing("192.168.1.20:8080", "sha256/test")
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	deviceName := "Tony's iPhone"
	proofMessage := []byte(strings.Join([]string{started.PairingID, started.OneTimeSecret, "gw_test", deviceName}, "\n"))
	deviceID, access, refresh, err := service.Complete(context.Background(), started.PairingID, deviceName, publicKey, ed25519.Sign(privateKey, proofMessage))
	if err != nil || deviceID == "" || access == "" || refresh == "" {
		t.Fatalf("complete = %q, %q, %q, %v", deviceID, access, refresh, err)
	}
	authorizedDeviceID, err := service.Authorize(context.Background(), access)
	if err != nil || authorizedDeviceID != deviceID {
		t.Fatalf("authorize = %q, %v", authorizedDeviceID, err)
	}
	newAccess, newRefresh, err := service.Refresh(context.Background(), refresh)
	if err != nil || newAccess == access || newRefresh == refresh {
		t.Fatalf("refresh = %q, %q, %v", newAccess, newRefresh, err)
	}
	if _, _, err := service.Refresh(context.Background(), refresh); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("refresh token was reusable: %v", err)
	}
	if err := service.Revoke(context.Background(), deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authorize(context.Background(), newAccess); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked authorization = %v", err)
	}
}

func TestPairingIsLocalOnlyAndSingleUse(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := NewService("gw_test", database)
	if _, err := service.StartPairing("8.8.8.8:443", "sha256/test"); err == nil {
		t.Fatal("remote pairing was accepted")
	}
	started, err := service.StartPairing("[fe80::1]:8080", "sha256/test")
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte(strings.Join([]string{started.PairingID, started.OneTimeSecret, "gw_test", "iPhone"}, "\n"))
	if _, _, _, err := service.Complete(context.Background(), started.PairingID, "iPhone", publicKey, ed25519.Sign(privateKey, message)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.Complete(context.Background(), started.PairingID, "iPhone", publicKey, ed25519.Sign(privateKey, message)); !errors.Is(err, ErrPairingExpired) {
		t.Fatalf("second completion = %v", err)
	}
}

func TestTailnetPairingAcceptsTailscalePeerButNotPublicAddress(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := NewService("gw_test", database)
	if _, err := service.StartPairingFromTailnet("100.105.36.112:443", "sha256/test"); err != nil {
		t.Fatalf("Tailscale IPv4 pairing = %v", err)
	}
	if _, err := service.StartPairingFromTailnet("[fd7a:115c:a1e0::152a:2471]:443", "sha256/test"); err != nil {
		t.Fatalf("Tailscale IPv6 pairing = %v", err)
	}
	if _, err := service.StartPairingFromTailnet("8.8.8.8:443", "sha256/test"); err == nil {
		t.Fatal("public pairing was accepted")
	}
}

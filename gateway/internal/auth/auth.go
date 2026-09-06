package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/id"
)

const (
	PairingLifetime = 5 * time.Minute
	AccessLifetime  = 15 * time.Minute
	RefreshLifetime = 30 * 24 * time.Hour
)

var (
	ErrPairingExpired = errors.New("pairing expired")
	ErrInvalidProof   = errors.New("invalid pairing proof")
	ErrDeviceRevoked  = errors.New("device revoked")
	ErrInvalidToken   = errors.New("invalid token")
)

type TokenKind string

const (
	AccessToken  TokenKind = "access"
	RefreshToken TokenKind = "refresh"
)

type TokenStore interface {
	CreateDevice(context.Context, string, string, []byte) error
	StoreToken(context.Context, []byte, string, string, time.Time, time.Time) error
	LookupToken(context.Context, []byte, string, time.Time) (string, error)
	RevokeToken(context.Context, []byte, string) error
	RevokeDevice(context.Context, string) error
}

type Service struct {
	GatewayID string
	Store     TokenStore
	Now       func() time.Time
	mu        sync.Mutex
	pairings  map[string]pairing
}

type pairing struct {
	ID        string
	Secret    string
	ExpiresAt time.Time
}

type PairingStart struct {
	GatewayID     string `json:"gatewayId"`
	PairingID     string `json:"pairingId"`
	OneTimeSecret string `json:"oneTimeSecret"`
	ExpiresAt     int64  `json:"expiresAt"`
	Fingerprint   string `json:"fingerprint"`
}

func NewService(gatewayID string, store TokenStore) *Service {
	return &Service{GatewayID: gatewayID, Store: store, Now: time.Now, pairings: make(map[string]pairing)}
}

func (s *Service) StartPairing(remoteAddr, fingerprint string) (PairingStart, error) {
	if !isLocalAddress(remoteAddr) {
		return PairingStart{}, fmt.Errorf("pairing is local-only")
	}
	return s.startPairing(fingerprint)
}

// StartPairingFromTailnet is the Tailnet-only counterpart to StartPairing.
// Tailscale assigns peers from 100.64.0.0/10 (and its ULA range), which is
// not RFC1918 and therefore must not be rejected as a public client. Serve
// proxies into the Gateway's loopback listener, so the backend may observe
// either the Tailnet peer address or a loopback address. The API layer calls
// this only for the loopback-backed Tailscale Serve listener; public fallback
// remains disabled.
func (s *Service) StartPairingFromTailnet(remoteAddr, fingerprint string) (PairingStart, error) {
	if !isLoopbackAddress(remoteAddr) && !isTailnetAddress(remoteAddr) {
		return PairingStart{}, fmt.Errorf("pairing is local-only")
	}
	return s.startPairing(fingerprint)
}

func (s *Service) startPairing(fingerprint string) (PairingStart, error) {
	pairingID, err := id.New("pair_", 16)
	if err != nil {
		return PairingStart{}, err
	}
	secret, err := randomToken(32)
	if err != nil {
		return PairingStart{}, err
	}
	expiresAt := s.Now().Add(PairingLifetime)
	s.mu.Lock()
	s.pairings[pairingID] = pairing{ID: pairingID, Secret: secret, ExpiresAt: expiresAt}
	s.mu.Unlock()
	return PairingStart{GatewayID: s.GatewayID, PairingID: pairingID, OneTimeSecret: secret, ExpiresAt: expiresAt.Unix(), Fingerprint: fingerprint}, nil
}

// Complete verifies a signature over the one-time secret and consumes the
// pairing record before issuing credentials. This prevents replay.
func (s *Service) Complete(ctx context.Context, pairingID, deviceName string, publicKey ed25519.PublicKey, proof []byte) (string, string, string, error) {
	s.mu.Lock()
	record, ok := s.pairings[pairingID]
	if ok {
		delete(s.pairings, pairingID)
	}
	s.mu.Unlock()
	if !ok || !s.Now().Before(record.ExpiresAt) {
		return "", "", "", ErrPairingExpired
	}
	message := []byte(strings.Join([]string{pairingID, record.Secret, s.GatewayID, deviceName}, "\n"))
	if !ed25519.Verify(publicKey, message, proof) {
		return "", "", "", ErrInvalidProof
	}
	deviceID, err := id.New("dev_", 16)
	if err != nil {
		return "", "", "", err
	}
	if err := s.Store.CreateDevice(ctx, deviceID, deviceName, publicKey); err != nil {
		return "", "", "", err
	}
	access, refresh, err := s.issueTokens(ctx, deviceID)
	return deviceID, access, refresh, err
}

func (s *Service) issueTokens(ctx context.Context, deviceID string) (string, string, error) {
	now := s.Now()
	access, err := randomToken(32)
	if err != nil {
		return "", "", err
	}
	refresh, err := randomToken(32)
	if err != nil {
		return "", "", err
	}
	if err := s.Store.StoreToken(ctx, hash(access), deviceID, string(AccessToken), now, now.Add(AccessLifetime)); err != nil {
		return "", "", err
	}
	if err := s.Store.StoreToken(ctx, hash(refresh), deviceID, string(RefreshToken), now, now.Add(RefreshLifetime)); err != nil {
		return "", "", err
	}
	return access, refresh, nil
}

func (s *Service) Authorize(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrInvalidToken
	}
	deviceID, err := s.Store.LookupToken(ctx, hash(token), string(AccessToken), s.Now())
	if err != nil {
		if errors.Is(err, ErrDeviceRevoked) {
			return "", err
		}
		return "", ErrInvalidToken
	}
	return deviceID, nil
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (string, string, error) {
	deviceID, err := s.Store.LookupToken(ctx, hash(refreshToken), string(RefreshToken), s.Now())
	if err != nil {
		return "", "", ErrInvalidToken
	}
	if err := s.Store.RevokeToken(ctx, hash(refreshToken), string(RefreshToken)); err != nil {
		return "", "", err
	}
	return s.issueTokens(ctx, deviceID)
}

func (s *Service) Revoke(ctx context.Context, deviceID string) error {
	return s.Store.RevokeDevice(ctx, deviceID)
}

func randomToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hash(token string) []byte {
	result := sha256.Sum256([]byte(token))
	return result[:]
}

func isLocalAddress(value string) bool {
	ip := parseAddress(value)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func isLoopbackAddress(value string) bool {
	ip := parseAddress(value)
	return ip != nil && ip.IsLoopback()
}

func parseAddress(value string) net.IP {
	host, _, err := net.SplitHostPort(value)
	if err != nil {
		host = value
	}
	return net.ParseIP(host)
}

func isTailnetAddress(value string) bool {
	ip := parseAddress(value)
	if ip == nil {
		return false
	}
	// Tailscale IPv4 peers use the CGNAT block, not RFC1918. Tailscale IPv6
	// peers use a ULA address and are already covered by IsPrivate.
	_, tailnetIPv4, _ := net.ParseCIDR("100.64.0.0/10")
	return tailnetIPv4.Contains(ip) || ip.IsPrivate()
}

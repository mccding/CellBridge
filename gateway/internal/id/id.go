package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func New(prefix string, bytes int) (string, error) {
	if bytes < 1 {
		return "", fmt.Errorf("id byte length must be positive")
	}
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(raw), nil
}

package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Target struct {
	DeviceID    string
	APNSToken   string
	VoIPToken   string
	Environment string
	Locale      string
}

type VoIPPayload struct {
	CallUUID  string `json:"callUUID"`
	CallID    string `json:"callId"`
	Handle    string `json:"handle"`
	GatewayID string `json:"gatewayId"`
	IssuedAt  int64  `json:"issuedAt"`
}

type MessagePayload struct {
	MessageID string `json:"messageId"`
	GatewayID string `json:"gatewayId"`
	IssuedAt  int64  `json:"issuedAt"`
}

type Sender interface {
	SendVoIP(context.Context, Target, VoIPPayload) error
	SendMessage(context.Context, Target, MessagePayload) error
}

// Broker is the Gateway-side client for the optional CellBridge Push Broker.
// The broker is deliberately kept behind Sender so a deployment can replace
// it with a direct APNs implementation without touching call/SMS code.
type Broker struct {
	BaseURL       string
	Authorization string
	Client        *http.Client
}

func NewBroker(baseURL, authorization string) *Broker {
	return &Broker{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), Authorization: authorization, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (b *Broker) SendVoIP(ctx context.Context, target Target, payload VoIPPayload) error {
	return b.post(ctx, "/v1/push/voip", target, payload)
}

func (b *Broker) SendMessage(ctx context.Context, target Target, payload MessagePayload) error {
	return b.post(ctx, "/v1/push/message", target, payload)
}

func (b *Broker) post(ctx context.Context, path string, target Target, payload any) error {
	if b == nil || b.BaseURL == "" {
		return fmt.Errorf("push broker is not configured")
	}
	if target.DeviceID == "" || target.Environment == "" {
		return fmt.Errorf("push target is invalid")
	}
	token := target.APNSToken
	if strings.HasSuffix(path, "/voip") {
		token = target.VoIPToken
	}
	if token == "" {
		return fmt.Errorf("push target token is missing")
	}
	body, err := json.Marshal(struct {
		DeviceID    string `json:"deviceId"`
		Token       string `json:"token"`
		Environment string `json:"environment"`
		Locale      string `json:"locale"`
		Payload     any    `json:"payload"`
	}{DeviceID: target.DeviceID, Token: token, Environment: target.Environment, Locale: target.Locale, Payload: payload})
	if err != nil {
		return fmt.Errorf("encode push request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create push request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if b.Authorization != "" {
		request.Header.Set("Authorization", b.Authorization)
	}
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("send push request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("push broker returned HTTP %d", response.StatusCode)
	}
	return nil
}

package push

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrokerSendsVoIPEnvelopeWithoutSMSBody(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/push/voip" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests <- body
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	broker := NewBroker(server.URL, "Bearer broker-test")
	err := broker.SendVoIP(context.Background(), Target{DeviceID: "dev_1", VoIPToken: "voip-token", Environment: "sandbox", Locale: "zh-CN"}, VoIPPayload{CallUUID: "00000000-0000-4000-8000-000000000001", CallID: "call_1", Handle: "+8613800138000", GatewayID: "gw_1", IssuedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	body := <-requests
	if body["token"] != "voip-token" || body["deviceId"] != "dev_1" {
		t.Fatalf("unexpected target: %#v", body)
	}
	payload, ok := body["payload"].(map[string]any)
	if !ok || payload["callUUID"] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("unexpected payload: %#v", body["payload"])
	}
}

func TestBrokerRequiresTheCorrectToken(t *testing.T) {
	broker := NewBroker("http://127.0.0.1:1", "")
	err := broker.SendMessage(context.Background(), Target{DeviceID: "dev_1", Environment: "sandbox"}, MessagePayload{MessageID: "msg_1"})
	if err == nil {
		t.Fatal("expected missing APNs token error")
	}
}

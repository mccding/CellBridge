package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testAgent() *agent {
	return newAgent(config{
		BindAddr:    "127.0.0.1:18788",
		Port:        18788,
		ServiceIP:   net.ParseIP("127.0.0.1"),
		ServiceName: "CellBridge QDC507",
		DeviceID:    "test-device",
		Token:       "test-token",
	})
}

func TestIdentityAndHealthAreDiscoveryReadable(t *testing.T) {
	server := httptest.NewServer(newMux(testAgent()))
	defer server.Close()
	identity := getRequest(t, server.URL+"/v1/identity", "")
	if identity.Code != http.StatusOK || !strings.Contains(identity.Body.String(), `"deviceFamily":"qdc507"`) {
		t.Fatalf("identity response: %d %s", identity.Code, identity.Body.String())
	}
	health := getRequest(t, server.URL+"/v1/health", "")
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"state":"degraded"`) {
		t.Fatalf("health response: %d %s", health.Code, health.Body.String())
	}
}

func TestMutationsRequirePairingTokenAndFailClosed(t *testing.T) {
	agent := testAgent()
	server := httptest.NewServer(newMux(agent))
	defer server.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"to":"10086","body":"test"}`))
	request.Header.Set("Content-Type", "application/json")
	unauthorized := httptest.NewRecorder()
	newMux(agent).ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status: %d", unauthorized.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"to":"10086","body":"test"}`))
	request.Header.Set("Authorization", "Bearer test-token")
	authorized := httptest.NewRecorder()
	newMux(agent).ServeHTTP(authorized, request)
	if authorized.Code != http.StatusNotImplemented {
		t.Fatalf("expected fail-closed status, got %d: %s", authorized.Code, authorized.Body.String())
	}
}

func getRequest(t *testing.T, url, token string) *httptest.ResponseRecorder {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	recorder.Code = response.StatusCode
	_, _ = recorder.Body.ReadFrom(response.Body)
	return recorder
}

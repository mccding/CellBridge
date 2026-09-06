package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/auth"
	"github.com/cellbridge/cellbridge/gateway/internal/call"
	"github.com/cellbridge/cellbridge/gateway/internal/db"
	"github.com/cellbridge/cellbridge/gateway/internal/recording"
)

func TestReadOnlyV1Endpoints(t *testing.T) {
	server := NewServer("gw_test", "Test Gateway", "dev")
	handler := server.Handler()

	tests := []struct {
		path       string
		wantStatus int
	}{
		{path: "/api/v1/health", wantStatus: http.StatusOK},
		{path: "/api/v1/gateway", wantStatus: http.StatusOK},
		{path: "/api/v1/line", wantStatus: http.StatusOK},
		{path: "/api/v1/missing", wantStatus: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantStatus == http.StatusOK {
				var body map[string]any
				if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
					t.Fatalf("decode JSON: %v", err)
				}
			}
		})
	}
}

type fakeCallModem struct {
	mu      sync.Mutex
	dials   int
	answers int
	hangups int
	dtmfs   int
}

func (m *fakeCallModem) Dial(context.Context, string) error {
	m.mu.Lock()
	m.dials++
	m.mu.Unlock()
	return nil
}
func (m *fakeCallModem) Answer(context.Context) error {
	m.mu.Lock()
	m.answers++
	m.mu.Unlock()
	return nil
}
func (m *fakeCallModem) Hangup(context.Context) error {
	m.mu.Lock()
	m.hangups++
	m.mu.Unlock()
	return nil
}
func (m *fakeCallModem) DTMF(context.Context, rune) error {
	m.mu.Lock()
	m.dtmfs++
	m.mu.Unlock()
	return nil
}

func TestCallAPIIsIdempotentAndPersistsState(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.Database = database
	application.Auth = auth.NewService("gw_test", database)
	application.Fingerprint = "sha256/test"
	application.CallControl = call.NewController()
	modemClient := &fakeCallModem{}
	application.CallModem = modemClient
	handler := application.Handler()
	accessToken := pairTestDevice(t, handler)

	body := `{"to":"+8613800138000","clientCallId":"client-call-1"}`
	first := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Idempotency-Key", "dial-1")
	handler.ServeHTTP(first, request)
	if first.Code != http.StatusCreated {
		t.Fatalf("dial status = %d: %s", first.Code, first.Body.String())
	}
	firstBody := first.Body.String()
	var created callResponse
	if err := json.Unmarshal([]byte(firstBody), &created); err != nil {
		t.Fatal(err)
	}
	if created.GatewayID != "gw_test" || created.LineID != "gw_test:line" {
		t.Fatalf("created call route metadata = gateway=%q line=%q", created.GatewayID, created.LineID)
	}
	replay := httptest.NewRecorder()
	replayRequest := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	replayRequest.Header.Set("Authorization", "Bearer "+accessToken)
	replayRequest.Header.Set("Idempotency-Key", "dial-1")
	handler.ServeHTTP(replay, replayRequest)
	if replay.Code != http.StatusCreated || replay.Body.String() != firstBody {
		t.Fatalf("replay = %d %q", replay.Code, replay.Body.String())
	}
	modemClient.mu.Lock()
	dials := modemClient.dials
	modemClient.mu.Unlock()
	if dials != 1 {
		t.Fatalf("modem dial count = %d", dials)
	}

	hangup := httptest.NewRecorder()
	hangupRequest := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+created.ID+"/hangup", nil)
	hangupRequest.Header.Set("Authorization", "Bearer "+accessToken)
	hangupRequest.Header.Set("Idempotency-Key", "hangup-1")
	handler.ServeHTTP(hangup, hangupRequest)
	if hangup.Code != http.StatusNoContent {
		t.Fatalf("hangup status = %d: %s", hangup.Code, hangup.Body.String())
	}
	get := httptest.NewRecorder()
	getRequest := httptest.NewRequest(http.MethodGet, "/api/v1/calls/"+created.ID, nil)
	getRequest.Header.Set("Authorization", "Bearer "+accessToken)
	handler.ServeHTTP(get, getRequest)
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"state":"ending"`) {
		t.Fatalf("get call = %d: %s", get.Code, get.Body.String())
	}
	list := httptest.NewRecorder()
	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/calls?limit=10", nil)
	listRequest.Header.Set("Authorization", "Bearer "+accessToken)
	handler.ServeHTTP(list, listRequest)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), created.ID) ||
		!strings.Contains(list.Body.String(), `"gatewayID":"gw_test"`) ||
		!strings.Contains(list.Body.String(), `"lineID":"gw_test:line"`) {
		t.Fatalf("list calls = %d: %s", list.Code, list.Body.String())
	}
}

func TestCallDeleteIsIdempotentAndSynced(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "delete-call.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	endedAt := time.Now()
	callID := "completed-call-1"
	if _, err := database.SaveCall(context.Background(), db.Call{
		ID: callID, Direction: "outbound", Peer: "+8613800138000", State: "ended",
		StartedAt: endedAt.Add(-time.Minute), EndedAt: &endedAt, EndReason: "remote_ended",
	}); err != nil {
		t.Fatal(err)
	}
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.Database = database
	application.Auth = auth.NewService("gw_test", database)
	application.Fingerprint = "sha256/test"
	handler := application.Handler()
	accessToken := pairTestDevice(t, handler)

	remove := httptest.NewRecorder()
	removeRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/calls/"+callID, nil)
	removeRequest.Header.Set("Authorization", "Bearer "+accessToken)
	removeRequest.Header.Set("Idempotency-Key", "call-delete-1")
	handler.ServeHTTP(remove, removeRequest)
	if remove.Code != http.StatusNoContent {
		t.Fatalf("delete call = %d: %s", remove.Code, remove.Body.String())
	}

	replay := httptest.NewRecorder()
	replayRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/calls/"+callID, nil)
	replayRequest.Header.Set("Authorization", "Bearer "+accessToken)
	replayRequest.Header.Set("Idempotency-Key", "call-delete-1")
	handler.ServeHTTP(replay, replayRequest)
	if replay.Code != http.StatusNoContent {
		t.Fatalf("delete call replay = %d: %s", replay.Code, replay.Body.String())
	}

	list := httptest.NewRecorder()
	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/calls?limit=10", nil)
	listRequest.Header.Set("Authorization", "Bearer "+accessToken)
	handler.ServeHTTP(list, listRequest)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), callID) {
		t.Fatalf("deleted call still listed = %d: %s", list.Code, list.Body.String())
	}

	changes, err := database.Sync(context.Background(), 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundDelete := false
	for _, change := range changes {
		if change.EntityType == "call" && change.EntityID == callID && change.Operation == "delete" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatalf("call delete was not added to sync log: %#v", changes)
	}
}

func TestDialRejectsEmergencyNumbers(t *testing.T) {
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.CallControl = call.NewController()
	modemClient := &fakeCallModem{}
	application.CallModem = modemClient
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(`{"to":"110","clientCallId":"client-call-1"}`))
	request.Header.Set("Idempotency-Key", "dial-emergency")
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "CB-CALL-011") {
		t.Fatalf("emergency dial = %d: %s", response.Code, response.Body.String())
	}
	modemClient.mu.Lock()
	defer modemClient.mu.Unlock()
	if modemClient.dials != 0 {
		t.Fatalf("emergency dial reached modem %d times", modemClient.dials)
	}
}

func pairTestDevice(t *testing.T, handler http.Handler) string {
	t.Helper()
	start := httptest.NewRequest(http.MethodPost, "/api/v1/pairing/start", nil)
	start.RemoteAddr = "192.168.1.20:54321"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, start)
	if response.Code != http.StatusOK {
		t.Fatalf("pair start = %d: %s", response.Code, response.Body.String())
	}
	var started auth.PairingStart
	if err := json.NewDecoder(response.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	deviceName := "Call Test iPhone"
	proof := ed25519.Sign(privateKey, []byte(strings.Join([]string{started.PairingID, started.OneTimeSecret, "gw_test", deviceName}, "\n")))
	completeBody, _ := json.Marshal(map[string]string{
		"pairingId": started.PairingID, "deviceName": deviceName,
		"devicePublicKey": base64.RawStdEncoding.EncodeToString(publicKey),
		"proof":           base64.RawStdEncoding.EncodeToString(proof),
	})
	complete := httptest.NewRecorder()
	completeRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pairing/complete", strings.NewReader(string(completeBody)))
	handler.ServeHTTP(complete, completeRequest)
	if complete.Code != http.StatusOK {
		t.Fatalf("pair complete = %d: %s", complete.Code, complete.Body.String())
	}
	var credentials struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(complete.Body).Decode(&credentials); err != nil {
		t.Fatal(err)
	}
	return credentials.AccessToken
}

func TestV1IsReadOnlyUntilAuthAndMutationLayersLand(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/line", nil)
	response := httptest.NewRecorder()
	NewServer("gw_test", "Test Gateway", "dev").Handler().ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestPairingAndAuthorizedSync(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	authentication := auth.NewService("gw_test", database)
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.Database = database
	application.Auth = authentication
	application.Fingerprint = "sha256/test"
	handler := application.Handler()

	startRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pairing/start", nil)
	startRequest.RemoteAddr = "192.168.1.20:54321"
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusOK {
		t.Fatalf("pairing start status = %d: %s", startResponse.Code, startResponse.Body.String())
	}
	var started auth.PairingStart
	if err := json.NewDecoder(startResponse.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	deviceName := "Test iPhone"
	proofPayload := []byte(strings.Join([]string{started.PairingID, started.OneTimeSecret, "gw_test", deviceName}, "\n"))
	completeBody, _ := json.Marshal(map[string]string{
		"pairingId": started.PairingID, "deviceName": deviceName,
		"devicePublicKey": base64.RawStdEncoding.EncodeToString(publicKey),
		"proof":           base64.RawStdEncoding.EncodeToString(ed25519.Sign(privateKey, proofPayload)),
	})
	completeRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pairing/complete", strings.NewReader(string(completeBody)))
	completeResponse := httptest.NewRecorder()
	handler.ServeHTTP(completeResponse, completeRequest)
	if completeResponse.Code != http.StatusOK {
		t.Fatalf("pairing complete status = %d: %s", completeResponse.Code, completeResponse.Body.String())
	}
	var credentials struct {
		DeviceID    string `json:"deviceId"`
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(completeResponse.Body).Decode(&credentials); err != nil {
		t.Fatal(err)
	}
	if credentials.DeviceID == "" || credentials.AccessToken == "" {
		t.Fatalf("credentials = %#v", credentials)
	}
	lineRequest := httptest.NewRequest(http.MethodGet, "/api/v1/line", nil)
	lineRequest.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	lineResponse := httptest.NewRecorder()
	handler.ServeHTTP(lineResponse, lineRequest)
	if lineResponse.Code != http.StatusOK {
		t.Fatalf("authorized line status = %d", lineResponse.Code)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/line", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized line status = %d", unauthorized.Code)
	}
}

func TestEmptySyncReturnsEmptyArray(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.Database = database
	application.Auth = auth.NewService("gw_test", database)
	application.Fingerprint = "sha256/test"
	handler := application.Handler()
	accessToken := pairTestDevice(t, handler)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/sync?after=0&limit=500", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("empty sync status = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Changes json.RawMessage `json:"changes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if string(payload.Changes) != "[]" {
		t.Fatalf("empty sync changes = %s, want []", payload.Changes)
	}
}

func TestTailnetPairingStartAcceptsTailscalePeerOnly(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.Database = database
	application.Auth = auth.NewService("gw_test", database)
	application.Fingerprint = "sha256/test"
	application.NetworkMode = "tailnet"
	application.TailnetHostname = "cellbridge-nas.example.ts.net"
	handler := application.Handler()

	request := httptest.NewRequest(http.MethodPost, "/api/v1/pairing/start", nil)
	request.RemoteAddr = "100.105.36.112:443"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Tailnet pairing status = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "cellbridge-nas.example.ts.net") {
		t.Fatalf("Tailnet pairing omitted advertised URL: %s", response.Body.String())
	}

	loopbackRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pairing/start", nil)
	loopbackRequest.RemoteAddr = "127.0.0.1:54321"
	loopbackResponse := httptest.NewRecorder()
	handler.ServeHTTP(loopbackResponse, loopbackRequest)
	if loopbackResponse.Code != http.StatusOK {
		t.Fatalf("Serve loopback pairing status = %d: %s", loopbackResponse.Code, loopbackResponse.Body.String())
	}

	publicRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pairing/start", nil)
	publicRequest.RemoteAddr = "8.8.8.8:443"
	publicResponse := httptest.NewRecorder()
	handler.ServeHTTP(publicResponse, publicRequest)
	if publicResponse.Code != http.StatusForbidden {
		t.Fatalf("public pairing status = %d, want %d", publicResponse.Code, http.StatusForbidden)
	}
}

func TestMessageReadDeleteActionsAreAuthenticatedIdempotentAndSynced(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	messageID := "msg_action_1"
	if _, err := database.InsertMessage(context.Background(), db.Message{
		ID: messageID, ThreadKey: "+8613800138000", Direction: "inbound", Peer: "+8613800138000", Body: "待处理", Encoding: "ucs2", Status: "sent", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.Database = database
	application.Auth = auth.NewService("gw_test", database)
	application.Fingerprint = "sha256/test"
	handler := application.Handler()
	accessToken := pairTestDevice(t, handler)

	mark := httptest.NewRecorder()
	markRequest := httptest.NewRequest(http.MethodPost, "/api/v1/messages/"+messageID+"/read", nil)
	markRequest.Header.Set("Authorization", "Bearer "+accessToken)
	markRequest.Header.Set("Idempotency-Key", "message-read-1")
	handler.ServeHTTP(mark, markRequest)
	if mark.Code != http.StatusNoContent {
		t.Fatalf("mark read = %d: %s", mark.Code, mark.Body.String())
	}
	replay := httptest.NewRecorder()
	replayRequest := httptest.NewRequest(http.MethodPost, "/api/v1/messages/"+messageID+"/read", nil)
	replayRequest.Header.Set("Authorization", "Bearer "+accessToken)
	replayRequest.Header.Set("Idempotency-Key", "message-read-1")
	handler.ServeHTTP(replay, replayRequest)
	if replay.Code != http.StatusNoContent {
		t.Fatalf("mark read replay = %d: %s", replay.Code, replay.Body.String())
	}

	list := httptest.NewRecorder()
	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/messages?limit=10", nil)
	listRequest.Header.Set("Authorization", "Bearer "+accessToken)
	handler.ServeHTTP(list, listRequest)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"status":"read"`) ||
		!strings.Contains(list.Body.String(), `"gatewayID":"gw_test"`) ||
		!strings.Contains(list.Body.String(), `"lineID":"gw_test:line"`) {
		t.Fatalf("read message list = %d: %s", list.Code, list.Body.String())
	}

	remove := httptest.NewRecorder()
	removeRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/messages/"+messageID, nil)
	removeRequest.Header.Set("Authorization", "Bearer "+accessToken)
	removeRequest.Header.Set("Idempotency-Key", "message-delete-1")
	handler.ServeHTTP(remove, removeRequest)
	if remove.Code != http.StatusNoContent {
		t.Fatalf("delete message = %d: %s", remove.Code, remove.Body.String())
	}

	changes, err := database.Sync(context.Background(), 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundDelete := false
	for _, change := range changes {
		if change.EntityType == "message" && change.EntityID == messageID && change.Operation == "delete" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatalf("delete operation was not added to sync log: %#v", changes)
	}
}

func TestThreadMessagePaginationReturnsChronologicalPagesAndHasMore(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "thread-pages.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := time.UnixMilli(1_700_000_000_000)
	for index := 0; index < 205; index++ {
		id := "api-thread-msg-" + time.UnixMilli(int64(index)).Format("150405.000")
		if _, err := database.InsertMessage(context.Background(), db.Message{
			ID: id, ThreadKey: "+8613800138000", Direction: "inbound", Peer: "+8613800138000",
			Body: "消息", Encoding: "ucs2", Status: "sent", PDUHash: "hash-" + id,
			CreatedAt: base.Add(time.Duration(index) * time.Millisecond),
		}); err != nil {
			t.Fatal(err)
		}
	}
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.Database = database
	application.Auth = auth.NewService("gw_test", database)
	token := pairTestDevice(t, application.Handler())

	first := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodGet, "/api/v1/messages?threadKey=%2B8613800138000&limit=100", nil)
	firstRequest.Header.Set("Authorization", "Bearer "+token)
	application.Handler().ServeHTTP(first, firstRequest)
	if first.Code != http.StatusOK || first.Header().Get("X-CellBridge-Has-More") != "true" {
		t.Fatalf("first page = %d, hasMore=%q, body=%s", first.Code, first.Header().Get("X-CellBridge-Has-More"), first.Body.String())
	}
	var newest []messageResponse
	if err := json.NewDecoder(first.Body).Decode(&newest); err != nil {
		t.Fatal(err)
	}
	if len(newest) != 100 || newest[0].CreatedAt >= newest[len(newest)-1].CreatedAt {
		t.Fatalf("newest page = %d, first=%#v, last=%#v", len(newest), newest[0], newest[len(newest)-1])
	}

	older := httptest.NewRecorder()
	olderPath := "/api/v1/messages?threadKey=%2B8613800138000&limit=100&before=" + fmt.Sprint(newest[0].CreatedAt) + "&beforeId=" + newest[0].ID
	olderRequest := httptest.NewRequest(http.MethodGet, olderPath, nil)
	olderRequest.Header.Set("Authorization", "Bearer "+token)
	application.Handler().ServeHTTP(older, olderRequest)
	if older.Code != http.StatusOK {
		t.Fatalf("older page = %d: %s", older.Code, older.Body.String())
	}
	var previous []messageResponse
	if err := json.NewDecoder(older.Body).Decode(&previous); err != nil {
		t.Fatal(err)
	}
	if len(previous) != 100 || previous[len(previous)-1].CreatedAt >= newest[0].CreatedAt {
		t.Fatalf("older page leaked cursor: len=%d previous-last=%d newest-first=%d", len(previous), previous[len(previous)-1].CreatedAt, newest[0].CreatedAt)
	}
}

type apiRecordingEncoder struct{}

func (apiRecordingEncoder) Encode(_ context.Context, cellular, client, output string) error {
	left, err := os.ReadFile(cellular)
	if err != nil {
		return err
	}
	right, err := os.ReadFile(client)
	if err != nil {
		return err
	}
	return os.WriteFile(output, append(left, right...), 0o640)
}

func TestRecordingAPIIsAuthenticatedIdempotentAndRangeReadable(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Second)
	if _, err := database.SaveCall(context.Background(), db.Call{ID: "call-rec-1", Direction: "outbound", Peer: "+86138", State: "active", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	manager, err := recording.NewManagerWithEncoder(database, filepath.Join(t.TempDir(), "recordings"), 90, apiRecordingEncoder{})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetMinimumFreeBytes(0)
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.Database = database
	application.Auth = auth.NewService("gw_test", database)
	application.Fingerprint = "sha256/test"
	application.Recordings = manager
	handler := application.Handler()
	accessToken := pairTestDevice(t, handler)
	settings := httptest.NewRecorder()
	settingsRequest := httptest.NewRequest(http.MethodGet, "/api/v1/settings/recording", nil)
	settingsRequest.Header.Set("Authorization", "Bearer "+accessToken)
	handler.ServeHTTP(settings, settingsRequest)
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), `"autoMode":"off"`) {
		t.Fatalf("recording settings = %d: %s", settings.Code, settings.Body.String())
	}
	update := httptest.NewRecorder()
	updateRequest := httptest.NewRequest(http.MethodPut, "/api/v1/settings/recording", strings.NewReader(`{"retentionDays":30,"minimumFreeBytes":0}`))
	updateRequest.Header.Set("Authorization", "Bearer "+accessToken)
	handler.ServeHTTP(update, updateRequest)
	if update.Code != http.StatusOK || !strings.Contains(update.Body.String(), `"retentionDays":30`) {
		t.Fatalf("update recording settings = %d: %s", update.Code, update.Body.String())
	}
	auto := httptest.NewRecorder()
	autoRequest := httptest.NewRequest(http.MethodPut, "/api/v1/settings/recording", strings.NewReader(`{"autoMode":"all"}`))
	autoRequest.Header.Set("Authorization", "Bearer "+accessToken)
	handler.ServeHTTP(auto, autoRequest)
	if auto.Code != http.StatusConflict || !strings.Contains(auto.Body.String(), "CB-REC-013") {
		t.Fatalf("automatic recording settings = %d: %s", auto.Code, auto.Body.String())
	}

	start := httptest.NewRecorder()
	startRequest := httptest.NewRequest(http.MethodPost, "/api/v1/calls/call-rec-1/recording/start", nil)
	startRequest.Header.Set("Authorization", "Bearer "+accessToken)
	startRequest.Header.Set("Idempotency-Key", "recording-start-1")
	handler.ServeHTTP(start, startRequest)
	if start.Code != http.StatusCreated {
		t.Fatalf("recording start = %d: %s", start.Code, start.Body.String())
	}
	var startedRecording recordingResponse
	if err := json.Unmarshal(start.Body.Bytes(), &startedRecording); err != nil {
		t.Fatal(err)
	}
	if startedRecording.State != "recording" {
		t.Fatalf("start response = %#v", startedRecording)
	}
	if startedRecording.GatewayID != "gw_test" || startedRecording.LineID != "gw_test:line" {
		t.Fatalf("recording route metadata = gateway=%q line=%q", startedRecording.GatewayID, startedRecording.LineID)
	}
	manager.WriteCellularFrame("call-rec-1", make([]int16, 160))
	manager.WriteClientFrame("call-rec-1", []int16{900, -900})
	stop := httptest.NewRecorder()
	stopRequest := httptest.NewRequest(http.MethodPost, "/api/v1/calls/call-rec-1/recording/stop", nil)
	stopRequest.Header.Set("Authorization", "Bearer "+accessToken)
	stopRequest.Header.Set("Idempotency-Key", "recording-stop-1")
	handler.ServeHTTP(stop, stopRequest)
	if stop.Code != http.StatusAccepted || !strings.Contains(stop.Body.String(), `"state":"finalizing"`) {
		t.Fatalf("recording stop = %d: %s", stop.Code, stop.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		get := httptest.NewRecorder()
		getRequest := httptest.NewRequest(http.MethodGet, "/api/v1/recordings/"+startedRecording.ID, nil)
		getRequest.Header.Set("Authorization", "Bearer "+accessToken)
		handler.ServeHTTP(get, getRequest)
		if get.Code == http.StatusOK && strings.Contains(get.Body.String(), `"state":"ready"`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	audio := httptest.NewRecorder()
	audioRequest := httptest.NewRequest(http.MethodGet, "/api/v1/recordings/"+startedRecording.ID+"/audio", nil)
	audioRequest.Header.Set("Authorization", "Bearer "+accessToken)
	audioRequest.Header.Set("Range", "bytes=0-3")
	handler.ServeHTTP(audio, audioRequest)
	if audio.Code != http.StatusPartialContent || audio.Header().Get("Accept-Ranges") != "bytes" || audio.Header().Get("Cache-Control") != "private" {
		t.Fatalf("audio range = %d headers=%v", audio.Code, audio.Header())
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/recordings", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("recording list without auth = %d", unauthorized.Code)
	}
}

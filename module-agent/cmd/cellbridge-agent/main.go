package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cellbridge/cellbridge/module-agent/internal/control"
	"github.com/cellbridge/cellbridge/module-agent/internal/events"
	"github.com/cellbridge/cellbridge/module-agent/internal/mdns"
	"github.com/cellbridge/cellbridge/module-agent/internal/model"
)

const version = "0.1.0-h2-slice"

type config struct {
	BindAddr           string
	Port               uint16
	ServiceIP          net.IP
	ServiceName        string
	DeviceID           string
	Token              string
	RILPath            string
	EnableVerifiedCall bool
	EnableVerifiedSMS  bool
	AllowLoopback      bool
}

func loadConfig() (config, error) {
	bind := strings.TrimSpace(os.Getenv("CB_AGENT_BIND_ADDR"))
	if bind == "" {
		return config{}, errors.New("CB_AGENT_BIND_ADDR is required; wildcard bind is forbidden")
	}
	host, portText, err := net.SplitHostPort(bind)
	if err != nil || net.ParseIP(host) == nil {
		return config{}, errors.New("CB_AGENT_BIND_ADDR must be an explicit IP:port")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return config{}, errors.New("CB_AGENT_BIND_ADDR must contain a valid non-zero port")
	}
	hostIP := net.ParseIP(host).To4()
	ip := net.ParseIP(strings.TrimSpace(os.Getenv("CB_AGENT_SERVICE_IP")))
	if ip == nil {
		ip = hostIP
	}
	if ip.To4() == nil || !ip.To4().Equal(hostIP) {
		return config{}, errors.New("CB_AGENT_SERVICE_IP must be an IPv4 ECM address")
	}
	if host == "0.0.0.0" || host == "::" {
		return config{}, errors.New("wildcard listener is forbidden; bind to the ECM address")
	}
	allowLoopback := os.Getenv("CB_AGENT_ALLOW_LOOPBACK") == "1"
	if net.ParseIP(host).IsLoopback() && !allowLoopback {
		return config{}, errors.New("loopback listener is only allowed for an explicit local smoke test")
	}
	deviceID := strings.TrimSpace(os.Getenv("CB_AGENT_DEVICE_ID"))
	if deviceID == "" {
		return config{}, errors.New("CB_AGENT_DEVICE_ID is required")
	}
	if strings.ContainsAny(deviceID, " \t\r\n") {
		return config{}, errors.New("CB_AGENT_DEVICE_ID must be opaque and whitespace-free")
	}
	name := strings.TrimSpace(os.Getenv("CB_AGENT_SERVICE_NAME"))
	if name == "" {
		name = "CellBridge QDC507"
	}
	return config{
		BindAddr:           bind,
		Port:               uint16(port),
		ServiceIP:          ip.To4(),
		ServiceName:        name,
		DeviceID:           deviceID,
		Token:              strings.TrimSpace(os.Getenv("CB_AGENT_TOKEN")),
		RILPath:            strings.TrimSpace(os.Getenv("CB_AGENT_RIL_PATH")),
		EnableVerifiedCall: os.Getenv("CB_AGENT_ENABLE_VERIFIED_CALL") == "1",
		EnableVerifiedSMS:  os.Getenv("CB_AGENT_ENABLE_VERIFIED_SMS") == "1",
		AllowLoopback:      allowLoopback,
	}, nil
}

type agent struct {
	config   config
	identity model.Identity
	backend  control.Adapter
	journal  *events.Journal
	mu       sync.RWMutex
	health   model.Health
	line     model.Line
}

func newAgent(c config) *agent {
	backend := control.Adapter(&control.SimpleRILAdapter{Path: c.RILPath, EnableVerifiedCall: c.EnableVerifiedCall, EnableVerifiedSMS: c.EnableVerifiedSMS})
	state := "pairing_required"
	if c.Token != "" {
		state = "degraded"
	}
	callCapability := "blocked"
	if c.EnableVerifiedCall && c.RILPath != "" {
		callCapability = "ready"
	}
	smsCapability := "blocked"
	if c.EnableVerifiedSMS && c.RILPath != "" {
		smsCapability = "ready"
	}
	return &agent{
		config:   c,
		identity: model.Identity{DeviceID: c.DeviceID, DeviceFamily: "qdc507", AgentVersion: version, ProtocolVersion: 1},
		backend:  backend,
		journal:  events.NewJournal(256),
		health:   model.Health{State: state, Control: "qmi", SMS: smsCapability, Call: callCapability, VoiceRuntime: "unknown", Media: "unknown"},
		line:     model.Line{LineID: "module:" + c.DeviceID, SIM: "unknown", Registration: "unknown"},
	}
}

func (a *agent) refresh() {
	line, err := a.backend.Status(context.Background())
	a.mu.Lock()
	if err != nil {
		a.health.Control = "unavailable"
		a.health.State = "degraded"
	} else {
		line.LineID = a.line.LineID
		a.line = line
		a.health.Control = "qmi"
		if a.config.Token == "" {
			a.health.State = "pairing_required"
		} else if line.SIM == "ready" && line.Registration == "registered" {
			a.health.State = "degraded"
		}
	}
	current := a.line
	a.mu.Unlock()
	a.journal.Append("line.status", current, time.Now().Unix())
}

func (a *agent) identityHandler(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, a.identity)
}

func (a *agent) healthHandler(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	value := a.health
	a.mu.RUnlock()
	a.writeJSON(w, http.StatusOK, value)
}

func (a *agent) lineHandler(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	value := a.line
	a.mu.RUnlock()
	a.writeJSON(w, http.StatusOK, value)
}

func (a *agent) messagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.authorized(r) {
		a.writeError(w, http.StatusUnauthorized, "pairing required")
		return
	}
	var request struct {
		To   string `json:"to"`
		Body string `json:"body"`
	}
	if err := decodeBounded(r, &request); err != nil || strings.TrimSpace(request.To) == "" || strings.TrimSpace(request.Body) == "" {
		a.writeError(w, http.StatusBadRequest, "to and body are required")
		return
	}
	if err := a.backend.SendSMS(r.Context(), request.To, request.Body); err != nil {
		a.operationError(w, err)
		return
	}
	a.writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (a *agent) messagesSyncHandler(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		a.writeError(w, http.StatusUnauthorized, "pairing required")
		return
	}
	// The journal/database-backed SMS adapter is intentionally not claimed
	// until QMI WMS storage and unsolicited indications pass H2 HIL. Keeping
	// the route explicit prevents iOS from mistaking a missing route for a
	// network failure.
	a.writeError(w, http.StatusNotImplemented, "typed message sync is not verified yet")
}

func (a *agent) callsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.authorized(r) {
		a.writeError(w, http.StatusUnauthorized, "pairing required")
		return
	}
	var request struct {
		To           string `json:"to"`
		ClientCallID string `json:"clientCallId"`
	}
	if err := decodeBounded(r, &request); err != nil || strings.TrimSpace(request.To) == "" {
		a.writeError(w, http.StatusBadRequest, "to is required")
		return
	}
	value, err := a.backend.Dial(r.Context(), request.To)
	if err != nil {
		a.operationError(w, err)
		return
	}
	a.writeJSON(w, http.StatusAccepted, value)
}

func (a *agent) callAction(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		a.writeError(w, http.StatusUnauthorized, "pairing required")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/calls/"), "/"), "/")
	if len(parts) != 2 {
		a.writeError(w, http.StatusNotFound, "call action not found")
		return
	}
	callID, action := parts[0], parts[1]
	if r.Method != http.MethodPost {
		a.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var err error
	switch action {
	case "answer":
		err = a.backend.Answer(r.Context(), callID)
	case "hangup":
		err = a.backend.Hangup(r.Context(), callID)
	case "dtmf":
		var request struct {
			Digit string `json:"digit"`
		}
		if decodeBounded(r, &request) != nil || len(request.Digit) != 1 || !strings.Contains("0123456789*#", request.Digit) {
			a.writeError(w, http.StatusBadRequest, "invalid DTMF")
			return
		}
		err = a.backend.DTMF(r.Context(), callID, request.Digit)
	default:
		a.writeError(w, http.StatusNotFound, "call action not found")
		return
	}
	if err != nil {
		a.operationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *agent) eventsHandler(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		a.writeError(w, http.StatusUnauthorized, "pairing required")
		return
	}
	if strings.ToLower(r.Header.Get("Upgrade")) != "websocket" {
		a.writeError(w, http.StatusUpgradeRequired, "websocket required")
		return
	}
	lastSeq, _ := strconv.ParseInt(r.Header.Get("X-CellBridge-Last-Seq"), 10, 64)
	missing, ok := a.journal.Since(lastSeq)
	if !ok {
		a.writeError(w, http.StatusConflict, "event cursor is too old")
		return
	}
	connection, err := upgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer connection.Close()
	for _, event := range missing {
		if err := connection.writeJSON(event); err != nil {
			return
		}
	}
	stream, cancel := a.journal.Subscribe()
	defer cancel()
	for {
		select {
		case event, open := <-stream:
			if !open {
				return
			}
			if err := connection.writeJSON(event); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (a *agent) authorized(r *http.Request) bool {
	if a.config.Token == "" {
		return false
	}
	prefix := "Bearer "
	value := r.Header.Get("Authorization")
	return strings.HasPrefix(value, prefix) && value[len(prefix):] == a.config.Token
}

func (a *agent) operationError(w http.ResponseWriter, err error) {
	if errors.Is(err, control.ErrUnsupported) {
		a.writeError(w, http.StatusNotImplemented, "typed module operation is not verified yet")
		return
	}
	if errors.Is(err, control.ErrUnavailable) {
		a.writeError(w, http.StatusServiceUnavailable, "module control plane unavailable")
		return
	}
	a.writeError(w, http.StatusBadGateway, err.Error())
}

func (a *agent) writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *agent) writeError(w http.ResponseWriter, status int, message string) {
	a.writeJSON(w, status, map[string]string{"error": message})
}

func decodeBounded(r *http.Request, value interface{}) error {
	reader := io.LimitReader(r.Body, 64*1024+1)
	data, err := io.ReadAll(reader)
	if err != nil || len(data) > 64*1024 {
		return errors.New("request body too large")
	}
	return json.Unmarshal(data, value)
}

type webSocketConnection struct {
	connection net.Conn
	mu         sync.Mutex
}

func (c *webSocketConnection) Close() error { return c.connection.Close() }

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*webSocketConnection, error) {
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing websocket key", http.StatusBadRequest)
		return nil, errors.New("missing websocket key")
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unavailable", http.StatusNotImplemented)
		return nil, errors.New("hijacker unavailable")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	if buffered != nil {
		_ = buffered.Flush()
	}
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	response := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(hash[:]) + "\r\n\r\n"
	if _, err := io.WriteString(connection, response); err != nil {
		connection.Close()
		return nil, err
	}
	return &webSocketConnection{connection: connection}, nil
}

func (c *webSocketConnection) writeJSON(value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 64*1024 {
		return errors.New("websocket frame too large")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var header []byte
	switch {
	case len(data) < 126:
		header = []byte{0x81, byte(len(data))}
	case len(data) <= 65535:
		header = []byte{0x81, 126, byte(len(data) >> 8), byte(len(data))}
	default:
		header = []byte{0x81, 127, 0, 0, 0, 0, byte(len(data) >> 24), byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))}
	}
	if _, err := c.connection.Write(append(header, data...)); err != nil {
		return err
	}
	return nil
}

func newMux(a *agent) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/identity", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			a.writeError(w, 405, "method not allowed")
			return
		}
		a.identityHandler(w, r)
	})
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			a.writeError(w, 405, "method not allowed")
			return
		}
		a.healthHandler(w, r)
	})
	mux.HandleFunc("/v1/line", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			a.writeError(w, 405, "method not allowed")
			return
		}
		a.lineHandler(w, r)
	})
	mux.HandleFunc("/v1/messages", a.messagesHandler)
	mux.HandleFunc("/v1/messages/sync", a.messagesSyncHandler)
	mux.HandleFunc("/v1/calls/", a.callAction)
	mux.HandleFunc("/v1/calls", a.callsHandler)
	mux.HandleFunc("/v1/events", a.eventsHandler)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func main() {
	c, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	a := newAgent(c)
	// A typed modem operation may spend several seconds waiting for the
	// vendor QMI console to initialize and return its terminal result. Keep
	// the HTTP response alive longer than that bounded operation; otherwise
	// clients see an empty reply even though the Agent is still healthy.
	server := &http.Server{Addr: c.BindAddr, Handler: newMux(a), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 30 * time.Second}
	stop := make(chan struct{})
	go func() {
		if err := (mdns.Advertiser{Name: c.ServiceName, Host: "cellbridge-qdc507", IP: c.ServiceIP, Port: c.Port, DeviceID: c.DeviceID, AgentVersion: version}).Run(stop); err != nil {
			log.Printf("mDNS stopped: %v", err)
		}
	}()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.refresh()
			case <-stop:
				return
			}
		}
	}()
	listener, err := net.Listen("tcp", c.BindAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("cellbridge-agent %s listening on %s", version, c.BindAddr)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	// Start serving discovery and health immediately. The first QMI status
	// refresh is allowed to take several seconds and must not make pairing
	// requests race a listener that has not started yet.
	a.refresh()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	close(stop)
	_ = server.Close()
}

package at

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/id"
	"github.com/cellbridge/cellbridge/gateway/internal/modem"
	"github.com/cellbridge/cellbridge/gateway/internal/modem/qdc507"
	"github.com/google/uuid"
)

// Adapter is the conservative V1 AT command adapter. It deliberately keeps
// vendor-specific audio commands out of the control path; voice audio is
// provided by the selected VoiceAudio backend while this type owns calls,
// SMS, and line state.
type Adapter struct {
	client *Client
	mu     sync.Mutex
	active modem.CallID
	capMu  sync.RWMutex
	caps   modem.Capabilities
	events chan modem.ModemEvent
	close  sync.Once
}

func NewAdapter(client *Client) *Adapter {
	adapter := &Adapter{client: client, events: make(chan modem.ModemEvent, 32)}
	if client != nil {
		client.SetLineHandler(adapter.HandleURC)
	}
	return adapter
}

func (a *Adapter) Probe(ctx context.Context) (modem.Capabilities, error) {
	if a.client == nil {
		return modem.Capabilities{}, fmt.Errorf("AT client is unavailable")
	}
	manufacturer, err := firstPayload(a.client.Exchange(ctx, "AT+CGMI"))
	if err != nil {
		return modem.Capabilities{}, err
	}
	model, _ := firstPayload(a.client.Exchange(ctx, "AT+CGMM"))
	revision, _ := firstPayload(a.client.Exchange(ctx, "AT+CGMR"))
	capabilities := modem.Capabilities{
		Vendor:              manufacturer,
		Model:               model,
		Tier:                modem.CapabilitySMSOnly,
		SMS:                 true,
		FirmwareFingerprint: strings.TrimSpace(manufacturer + " " + model + " " + revision),
	}
	if voiceCapabilities, capabilityErr := qdc507.ProbeCapabilities(ctx, a.client, manufacturer, model); capabilityErr == nil {
		capabilities.Voice = voiceCapabilities.Voice
		capabilities.DTMF = voiceCapabilities.DTMF
		capabilities.Audio = voiceCapabilities.Audio
		capabilities.RequiresBootstrap = voiceCapabilities.RequiresBootstrap
		capabilities.Tier = voiceCapabilities.Tier
	}
	a.capMu.Lock()
	a.caps = capabilities
	a.capMu.Unlock()
	return capabilities, nil
}

func (a *Adapter) Status(ctx context.Context) (modem.LineStatus, error) {
	if a.client == nil {
		return modem.LineStatus{}, fmt.Errorf("AT client is unavailable")
	}
	simLines, err := a.client.Exchange(ctx, "AT+CPIN?")
	if err != nil {
		return modem.LineStatus{}, err
	}
	registrationLines, err := a.client.Exchange(ctx, "AT+CEREG?")
	if err != nil {
		return modem.LineStatus{}, err
	}
	signalLines, err := a.client.Exchange(ctx, "AT+CSQ")
	if err != nil {
		return modem.LineStatus{}, err
	}
	a.capMu.RLock()
	capabilities := a.caps
	a.capMu.RUnlock()
	if capabilities.Tier == "" {
		capabilities.Tier = modem.CapabilitySMSOnly
	}
	voiceState := "unavailable"
	if capabilities.Voice {
		voiceState = "control_only"
	}
	status := modem.LineStatus{
		SIM:            normalizeSIM(payload(simLines)),
		Registration:   normalizeRegistration(payload(registrationLines)),
		Signal:         modem.Signal{RSSI: parseRSSI(payload(signalLines))},
		CapabilityTier: capabilities.Tier,
		Voice:          voiceState,
		SMS:            "ready",
	}
	status.Signal.Bars = signalBars(status.Signal.RSSI)
	a.mu.Lock()
	if a.active != "" {
		active := a.active
		status.ActiveCallID = &active
	}
	a.mu.Unlock()
	return status, nil
}

func (a *Adapter) Dial(ctx context.Context, peer string) (modem.CallID, error) {
	a.mu.Lock()
	busy := a.active != ""
	a.mu.Unlock()
	if busy {
		return "", modem.ErrActiveCall
	}
	if _, err := a.client.Exchange(ctx, "ATD"+strings.TrimSpace(peer)+";"); err != nil {
		return "", err
	}
	callID, err := id.New("call_", 16)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.active = modem.CallID(callID)
	a.mu.Unlock()
	return modem.CallID(callID), nil
}

func (a *Adapter) Answer(ctx context.Context, callID modem.CallID) error {
	if err := a.validateActive(callID); err != nil {
		return err
	}
	_, err := a.client.Exchange(ctx, "ATA")
	return err
}

func (a *Adapter) Hangup(ctx context.Context, callID modem.CallID) error {
	if err := a.validateActive(callID); err != nil {
		return err
	}
	_, err := a.client.Exchange(ctx, "ATH")
	if err == nil {
		a.finishActive(callID, "local_hangup")
	}
	return err
}

func (a *Adapter) SendDTMF(ctx context.Context, callID modem.CallID, digit rune) error {
	if err := a.validateActive(callID); err != nil {
		return err
	}
	if !strings.ContainsRune("0123456789*#ABCD", digit) {
		return fmt.Errorf("invalid DTMF digit %q", digit)
	}
	_, err := a.client.Exchange(ctx, fmt.Sprintf("AT+VTS=\"%c\"", digit))
	return err
}

func (a *Adapter) SendSMS(ctx context.Context, destination string, payload modem.SMSPayload) (modem.SMSID, error) {
	destination = strings.TrimSpace(destination)
	if len(destination) <= 6 && payload.Body != "" {
		if _, err := a.client.Exchange(ctx, "AT+CMGF=1"); err != nil {
			return "", err
		}
		if _, err := a.client.Exchange(ctx, "AT+CSCS=\"GSM\""); err != nil {
			return "", err
		}
		if _, err := a.client.SendTextSMS(ctx, destination, payload.Body); err != nil {
			if errors.Is(err, ErrSubmissionResultUnknown) {
				return "", fmt.Errorf("%w: %v", modem.ErrSMSSubmissionUnknown, err)
			}
			return "", err
		}
		messageID, err := id.New("sms_", 16)
		return modem.SMSID(messageID), err
	}
	if payload.PDU == "" {
		return "", fmt.Errorf("SMS PDU is required")
	}
	if _, err := a.client.Exchange(ctx, "AT+CMGF=0"); err != nil {
		return "", err
	}
	pduBytes := len(strings.TrimSpace(payload.PDU)) / 2
	if pduBytes < 2 {
		return "", fmt.Errorf("invalid SMS PDU length")
	}
	// AT+CMGS takes TPDU octets, excluding the SMSC length octet.
	smscLength := 0
	if value, err := strconv.ParseInt(payload.PDU[:2], 16, 8); err == nil {
		smscLength = int(value)
	}
	tpduLength := pduBytes - 1 - smscLength
	if tpduLength < 1 {
		return "", fmt.Errorf("invalid SMS TPDU length")
	}
	if _, err := a.client.SendPDU(ctx, tpduLength, payload.PDU); err != nil {
		if errors.Is(err, ErrSubmissionResultUnknown) {
			return "", fmt.Errorf("%w: %v", modem.ErrSMSSubmissionUnknown, err)
		}
		return "", err
	}
	messageID, err := id.New("sms_", 16)
	return modem.SMSID(messageID), err
}

func (a *Adapter) ListSMS(ctx context.Context, cursor modem.SMSCursor) ([]modem.RawSMS, modem.SMSCursor, error) {
	if _, err := a.client.Exchange(ctx, "AT+CMGF=0"); err != nil {
		return nil, cursor, err
	}
	lines, err := a.client.Exchange(ctx, "AT+CMGL=4")
	if err != nil {
		return nil, cursor, err
	}
	var result []modem.RawSMS
	var current *modem.RawSMS
	lastIndex := strings.TrimSpace(string(cursor))
	for _, line := range lines {
		if strings.HasPrefix(line, "+CMGL:") {
			fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "+CMGL:")), ",")
			if len(fields) == 0 {
				continue
			}
			index, parseErr := strconv.Atoi(strings.TrimSpace(fields[0]))
			if parseErr != nil {
				continue
			}
			current = &modem.RawSMS{ModemStorage: "SM", ModemIndex: index}
			lastIndex = strconv.Itoa(index)
			continue
		}
		if current != nil && !IsFinal(line) && !strings.HasPrefix(line, "+") {
			pdu, decodeErr := hex.DecodeString(strings.TrimSpace(line))
			if decodeErr == nil {
				current.RawPDU = pdu
				result = append(result, *current)
			}
			current = nil
		}
	}
	return result, modem.SMSCursor(lastIndex), nil
}

func (a *Adapter) DeleteSMS(ctx context.Context, storageIndex string) error {
	if _, err := a.client.Exchange(ctx, "AT+CMGD="+strings.TrimSpace(storageIndex)); err != nil {
		return err
	}
	return nil
}

func (a *Adapter) Events() <-chan modem.ModemEvent { return a.events }

// WaitActive polls AT+CLCC until the outgoing call is answered (dir=0,
// state=0 active) or the context ends. Opening UAC capture before the
// cellular leg is active wedges the ALSA ASYNC stream into an XRUN that
// reads silence forever, so the SIP bridge must wait for this before
// starting audio.
func (a *Adapter) WaitActive(ctx context.Context) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
		lines, err := a.client.Exchange(ctx, "AT+CLCC")
		if err == nil {
			for _, line := range lines {
				// +CLCC: <id>,<dir>,<state>,... — answered is dir=0 outgoing, state=0 active
				if strings.HasPrefix(line, "+CLCC:") {
					fields := strings.Split(strings.TrimPrefix(line, "+CLCC:"), ",")
					if len(fields) >= 3 && strings.TrimSpace(fields[1]) == "0" && strings.TrimSpace(fields[2]) == "0" {
						return true, nil
					}
				}
			}
		}
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		}
	}
}

func (a *Adapter) Run(ctx context.Context) error {
	if a.client == nil {
		return fmt.Errorf("AT client is unavailable")
	}
	return a.client.Run(ctx)
}

// HandleURC is called by a serial reader when a line arrives outside an AT
// command exchange. It is public so a platform-specific reader can preserve
// unsolicited RING/NO CARRIER events without coupling it to the parser.
func (a *Adapter) HandleURC(line string) {
	// A URC may arrive exactly as Close() shuts the events channel
	// down (observed as a restart-time panic: "send on closed
	// channel"). Swallow it instead of crashing the gateway.
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Warn("adapter URC dropped during shutdown", "cause", recovered)
		}
	}()
	line = strings.TrimSpace(line)
	a.mu.Lock()
	callID := a.active
	a.mu.Unlock()
	var event modem.ModemEvent
	switch {
	case line == "RING":
		if callID == "" {
			callID = modem.CallID(uuid.NewString())
			a.mu.Lock()
			a.active = callID
			a.mu.Unlock()
		}
		event = modem.ModemEvent{Kind: "incoming", CallID: callID}
	case strings.Contains(line, "NO CARRIER"), strings.Contains(line, "BUSY"), strings.Contains(line, "NO ANSWER"):
		event = modem.ModemEvent{Kind: "ended", CallID: callID, Raw: line}
		// The modem terminated the call (e.g. an unanswered inbound call).
		// Clear the active marker here, or every later dial fails with
		// ErrActiveCall until the process restarts.
		a.mu.Lock()
		if a.active == callID && callID != "" {
			a.active = ""
		}
		a.mu.Unlock()
	default:
		return
	}
	select {
	case a.events <- event:
	default:
	}
}

func (a *Adapter) Close() error {
	var err error
	a.close.Do(func() {
		close(a.events)
		if a.client != nil {
			err = a.client.Close()
		}
	})
	return err
}

func (a *Adapter) validateActive(callID modem.CallID) error {
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active == "" || (callID != "" && active != callID) {
		return fmt.Errorf("active call mismatch")
	}
	return nil
}

func (a *Adapter) finishActive(callID modem.CallID, reason string) {
	a.mu.Lock()
	if a.active == callID {
		a.active = ""
	}
	a.mu.Unlock()
	select {
	case a.events <- modem.ModemEvent{Kind: "ended", CallID: callID, Raw: reason}:
	default:
	}
}

func firstPayload(lines []string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return payload(lines), nil
}

func payload(lines []string) string {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line != "OK" && !IsFinal(line) && !strings.HasPrefix(line, "AT") {
			return line
		}
	}
	return ""
}

func normalizeSIM(value string) string {
	value = strings.ToLower(value)
	switch {
	case strings.Contains(value, "ready"):
		return "ready"
	case strings.Contains(value, "pin"), strings.Contains(value, "puk"):
		return "locked"
	case strings.Contains(value, "not ready"):
		return "absent"
	default:
		return "unknown"
	}
}

func normalizeRegistration(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, ","); index >= 0 {
		value = strings.TrimSpace(value[index+1:])
	}
	switch value {
	case "1", "5":
		return "registered"
	case "2":
		return "searching"
	case "3":
		return "denied"
	default:
		return "unknown"
	}
}

func parseRSSI(value string) int {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, ":"); index >= 0 {
		value = strings.TrimSpace(value[index+1:])
	}
	if index := strings.Index(value, ","); index >= 0 {
		value = value[:index]
	}
	rssi, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || rssi == 99 {
		return 0
	}
	return -113 + rssi*2
}

func signalBars(rssi int) int {
	switch {
	case rssi >= -75:
		return 4
	case rssi >= -90:
		return 3
	case rssi >= -105:
		return 2
	case rssi > -113:
		return 1
	default:
		return 0
	}
}

var _ modem.ModemControl = (*Adapter)(nil)

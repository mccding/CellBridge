package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/module-agent/internal/model"
)

var (
	ErrUnsupported = errors.New("operation is not enabled by the verified module control adapter")
	ErrUnavailable = errors.New("module control plane is unavailable")

	// The vendor console owns the module's QMI client lifecycle. Keep every
	// invocation in this process serialized so a periodic health refresh cannot
	// race a future typed operation and corrupt the console session.
	simpleRILMu sync.Mutex
)

type Adapter interface {
	Status(context.Context) (model.Line, error)
	SendSMS(context.Context, string, string) error
	Dial(context.Context, string) (model.Call, error)
	Answer(context.Context, string) error
	Hangup(context.Context, string) error
	DTMF(context.Context, string, string) error
}

// SimpleRILAdapter is intentionally conservative. The QDC507 evidence proves
// that this vendor console reaches QMI and reports modem/network state. Only
// the call dial/end pair may be enabled after the explicit v8 business HIL;
// SMS, answer and DTMF remain fail-closed until their own typed HIL is done.
type SimpleRILAdapter struct {
	Path               string
	Timeout            time.Duration
	EnableVerifiedCall bool
	EnableVerifiedSMS  bool
	mu                 sync.Mutex
	active             *callSession
}

type callSession struct {
	reader *io.PipeReader
	input  *io.PipeWriter
	output *concurrentOutput
	cancel context.CancelFunc
	done   <-chan error
}

// smsSession is kept separate from callSession because SMS has a terminal
// QMI result for the submitted message, while a call remains interactive.
// Keeping the console alive until that result arrives avoids waiting for the
// vendor test binary to exit (which it does not reliably do after a command).
type smsSession struct {
	reader *io.PipeReader
	input  *io.PipeWriter
	output *concurrentOutput
	cancel context.CancelFunc
	done   <-chan error
}

type concurrentOutput struct {
	mu sync.Mutex
	b  strings.Builder
}

func (o *concurrentOutput) Write(value []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(value)
}

func (o *concurrentOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

func (a *SimpleRILAdapter) Status(ctx context.Context) (model.Line, error) {
	if a.Path == "" {
		return model.Line{}, ErrUnavailable
	}
	a.mu.Lock()
	busy := a.active != nil
	a.mu.Unlock()
	if busy {
		return model.Line{}, ErrUnavailable
	}
	if a.Timeout <= 0 {
		a.Timeout = 20 * time.Second
	}
	query := "modem status\nget_imsi 0 gw\nnw_cdma_info\nqmi_svc_versions\nquit\n"
	output, err := runConsole(ctx, a.Path, a.Timeout, query)
	if err != nil {
		return model.Line{}, err
	}
	line := model.Line{
		LineID:       "module:qdc507",
		SIM:          "unknown",
		Registration: "unknown",
		Signal:       0,
		Voice:        false,
		SMS:          false,
		DTMF:         false,
	}
	if strings.Contains(output, "modem is ONLINE") {
		line.Registration = "unknown"
	}
	if strings.Contains(output, "Registered with a network") {
		line.Registration = "registered"
	}
	if strings.Contains(output, "Registration denied") {
		line.Registration = "denied"
	}
	if strings.Contains(output, "Packet switch domain attach state") && strings.Contains(output, "Attached") {
		// Packet attach is useful evidence, but is not a substitute for SIM
		// readiness or a voice/SMS HIL. Keep the typed capabilities false.
		if line.Registration == "unknown" {
			line.Registration = "registered"
		}
	}
	if imsiPattern.MatchString(output) {
		line.SIM = "ready"
	}
	line.Signal = parseSignal(output)
	return line, nil
}

func (a *SimpleRILAdapter) SendSMS(ctx context.Context, to, body string) error {
	if !a.EnableVerifiedSMS {
		return ErrUnsupported
	}
	if a.Path == "" {
		return ErrUnavailable
	}
	pdu, err := buildGSMSMSSubmitPDU(to, body)
	if err != nil {
		return err
	}
	if a.Timeout <= 0 {
		a.Timeout = 20 * time.Second
	}
	simpleRILMu.Lock()
	defer simpleRILMu.Unlock()
	session, err := startSMSSession(ctx, a.Path, pdu)
	if err != nil {
		return err
	}
	accepted, qmiErr := waitForSMSResult(ctx, session.output, a.Timeout)
	stopSMSSession(session)
	if qmiErr == nil && accepted {
		return nil
	}
	// QMI path failed (observed: RESULT CODE 1 with empty cause). Fall back
	// to AT+CMGS via the module's AT port (AT SMS was proven to succeed with
	// the same PDU on this baseband). This keeps the verified SMS contract
	// while working around the vendor QMI WMS encapsulation incompatibility.
	if qmiErr != nil && !strings.Contains(qmiErr.Error(), "RESULT CODE 1") && !strings.Contains(qmiErr.Error(), "SMS rejected") {
		// For non-CODE-1 errors (e.g. unavailable), return directly.
		if !strings.Contains(qmiErr.Error(), "RESULT CODE") {
			return qmiErr
		}
	}
	if err := sendSMSViaAT(ctx, pdu, a.Timeout); err != nil {
		if qmiErr != nil {
			return fmt.Errorf("%w; AT fallback also failed: %v", qmiErr, err)
		}
		return err
	}
	return nil
}

func (a *SimpleRILAdapter) Dial(ctx context.Context, peer string) (model.Call, error) {
	if !a.EnableVerifiedCall {
		return model.Call{}, ErrUnsupported
	}
	peer = strings.TrimSpace(peer)
	if !phoneNumberPattern.MatchString(peer) {
		return model.Call{}, errors.New("dial target must contain 3 to 20 digits")
	}
	if a.Path == "" {
		return model.Call{}, ErrUnavailable
	}

	a.mu.Lock()
	if a.active != nil {
		a.mu.Unlock()
		return model.Call{}, errors.New("a module call is already active")
	}
	session, err := startCallSession(ctx, a.Path, peer)
	if err != nil {
		a.mu.Unlock()
		return model.Call{}, err
	}
	state, ok := waitForCallState(ctx, session.output, a.callTimeout())
	if !ok {
		stopCallSession(session)
		a.mu.Unlock()
		return model.Call{}, errors.New("dial returned no verified radio call state")
	}
	if state == "ended" {
		stopCallSession(session)
		a.mu.Unlock()
	} else {
		a.active = session
		a.mu.Unlock()
	}
	return model.Call{
		ID:        callIDForPeer(peer),
		LineID:    "module:qdc507",
		Peer:      peer,
		Direction: "outbound",
		State:     state,
	}, nil
}

func (a *SimpleRILAdapter) Answer(context.Context, string) error { return ErrUnsupported }

func (a *SimpleRILAdapter) Hangup(ctx context.Context, _ string) error {
	if !a.EnableVerifiedCall {
		return ErrUnsupported
	}
	if a.Path == "" {
		return ErrUnavailable
	}
	a.mu.Lock()
	session := a.active
	a.active = nil
	a.mu.Unlock()
	if session == nil {
		return ErrUnavailable
	}
	defer stopCallSession(session)
	if err := writeConsoleLine(ctx, session.input, "call_end"); err != nil {
		return err
	}
	if !waitForEndedState(ctx, session.output, a.callTimeout()) {
		return errors.New("hangup returned no verified radio END state")
	}
	return nil
}

func (a *SimpleRILAdapter) DTMF(context.Context, string, string) error { return ErrUnsupported }

func (a *SimpleRILAdapter) callTimeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return 15 * time.Second
}

func startCallSession(parent context.Context, path, peer string) (*callSession, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, path)
	reader, writer := io.Pipe()
	output := &concurrentOutput{}
	cmd.Stdin = reader
	cmd.Stdout = output
	cmd.Stderr = output
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()

	if filepath.Base(path) == "qmi_simple_ril_test" {
		if !waitConsole(parent, 5*time.Second) {
			cancel()
			_ = reader.Close()
			return nil, parent.Err()
		}
	}
	if err := writeConsoleLine(parent, writer, "dial "+peer); err != nil {
		cancel()
		_ = reader.Close()
		return nil, err
	}
	return &callSession{reader: reader, input: writer, output: output, cancel: cancel, done: done}, nil
}

func startSMSSession(parent context.Context, path, pdu string) (*smsSession, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, path)
	reader, writer := io.Pipe()
	output := &concurrentOutput{}
	cmd.Stdin = reader
	cmd.Stdout = output
	cmd.Stderr = output
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()

	if filepath.Base(path) == "qmi_simple_ril_test" {
		if !waitConsole(parent, 5*time.Second) {
			cancel()
			_ = writer.Close()
			_ = reader.Close()
			return nil, parent.Err()
		}
	}
	if err := writeConsoleLine(parent, writer, "mo_sms_gsm "+pdu); err != nil {
		cancel()
		_ = writer.Close()
		_ = reader.Close()
		return nil, err
	}
	return &smsSession{reader: reader, input: writer, output: output, cancel: cancel, done: done}, nil
}

func writeConsoleLine(ctx context.Context, writer *io.PipeWriter, line string) error {
	result := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, line+"\n")
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForCallState(ctx context.Context, output *concurrentOutput, timeout time.Duration) (string, bool) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if state, ok := parseCallState(output.String()); ok {
			return state, true
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-timer.C:
			return "", false
		case <-ticker.C:
		}
	}
}

func waitForEndedState(ctx context.Context, output *concurrentOutput, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if state, ok := parseCallState(output.String()); ok && state == "ended" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}

func stopCallSession(session *callSession) {
	if session == nil {
		return
	}
	quitContext, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = writeConsoleLine(quitContext, session.input, "quit")
	cancel()
	_ = session.input.Close()
	_ = session.reader.Close()
	session.cancel()
	select {
	case <-session.done:
	case <-time.After(3 * time.Second):
	}
}

func stopSMSSession(session *smsSession) {
	if session == nil {
		return
	}
	quitContext, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = writeConsoleLine(quitContext, session.input, "quit")
	cancel()
	_ = session.input.Close()
	_ = session.reader.Close()
	session.cancel()
	select {
	case <-session.done:
	case <-time.After(3 * time.Second):
	}
}

func waitForSMSResult(ctx context.Context, output *concurrentOutput, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		value := output.String()
		if result, ok := parseSMSTerminalResult(value); ok {
			if result.code == 0 {
				return true, nil
			}
			return false, fmt.Errorf("SMS rejected by QMI: %s", result.summary())
		}
		if strings.Contains(value, "SUCCESS (UNCONDITIONAL)") {
			return true, nil
		}
		if strings.Contains(value, "ERROR (") {
			return false, errors.New("SMS rejected by QMI")
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, fmt.Errorf("SMS result timeout after %s", timeout)
		case <-ticker.C:
		}
	}
}

type smsTerminalResult struct {
	code            int
	messageID       *int
	causeCodeValid  *int
	causeCode       *int
	errorClassValid *int
	errorClass      *int
}

func parseSMSTerminalResult(output string) (smsTerminalResult, bool) {
	code, ok := parseSMSInteger(output, "RESULT CODE")
	if !ok {
		return smsTerminalResult{}, false
	}
	return smsTerminalResult{
		code:            code,
		messageID:       parseSMSIntegerPtr(output, "MESSAGE ID"),
		causeCodeValid:  parseSMSIntegerPtr(output, "CAUSE CODE VALID"),
		causeCode:       parseSMSIntegerPtr(output, "CAUSE CODE"),
		errorClassValid: parseSMSIntegerPtr(output, "ERROR CLASS VALID"),
		errorClass:      parseSMSIntegerPtr(output, "ERROR CLASS"),
	}, true
}

func parseSMSIntegerPtr(output, label string) *int {
	value, ok := parseSMSInteger(output, label)
	if !ok {
		return nil
	}
	return &value
}

func parseSMSInteger(output, label string) (int, bool) {
	labelFields := strings.Fields(strings.ReplaceAll(label, "_", " "))
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.ReplaceAll(line, "_", " "))
		if len(fields) <= len(labelFields) {
			continue
		}
		matches := true
		for index, labelField := range labelFields {
			if !strings.EqualFold(fields[index], labelField) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		value, err := strconv.Atoi(fields[len(labelFields)])
		if err == nil {
			return value, true
		}
	}
	return 0, false
}

func (r smsTerminalResult) summary() string {
	parts := []string{fmt.Sprintf("RESULT CODE %d", r.code)}
	if r.messageID != nil {
		parts = append(parts, fmt.Sprintf("MESSAGE ID %d", *r.messageID))
	}
	if r.causeCodeValid != nil {
		parts = append(parts, fmt.Sprintf("CAUSE CODE VALID %d", *r.causeCodeValid))
	}
	if r.causeCode != nil {
		parts = append(parts, fmt.Sprintf("CAUSE CODE %d", *r.causeCode))
	}
	if r.errorClassValid != nil {
		parts = append(parts, fmt.Sprintf("ERROR CLASS VALID %d", *r.errorClassValid))
	}
	if r.errorClass != nil {
		parts = append(parts, fmt.Sprintf("ERROR CLASS %d", *r.errorClass))
	}
	return strings.Join(parts, "; ")
}

func runConsole(parent context.Context, path string, timeout time.Duration, input string) (string, error) {
	if filepath.Base(path) == "qmi_simple_ril_test" {
		simpleRILMu.Lock()
		defer simpleRILMu.Unlock()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	var closeInput io.Closer
	var inputDone <-chan struct{}
	if filepath.Base(path) == "qmi_simple_ril_test" {
		// The vendor console starts its QMI clients asynchronously. Sending the
		// whole stdin buffer at once races that initialization and intermittently
		// turns valid queries into INVALID ARGUMENT. Pace only this known vendor
		// console; test doubles and other executables keep the normal fast path.
		reader, writer := io.Pipe()
		cmd.Stdin = reader
		closeInput = reader
		done := make(chan struct{})
		inputDone = done
		go func() {
			defer close(done)
			feedSimpleRIL(ctx, writer, input)
		}()
	} else {
		cmd.Stdin = strings.NewReader(input)
	}
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	err := cmd.Run()
	if closeInput != nil {
		_ = closeInput.Close()
	}
	if inputDone != nil {
		<-inputDone
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout.String(), fmt.Errorf("simple RIL timeout after %s", timeout)
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("simple RIL: %w", err)
	}
	return stdout.String(), nil
}

func feedSimpleRIL(ctx context.Context, writer *io.PipeWriter, input string) {
	defer writer.Close()
	if !waitConsole(ctx, 5*time.Second) {
		return
	}
	for _, line := range strings.Split(input, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, err := io.WriteString(writer, line+"\n"); err != nil {
			return
		}
		if line == "quit" || !waitConsole(ctx, 1200*time.Millisecond) {
			return
		}
	}
}

func waitConsole(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

var signalPattern = regexp.MustCompile(`(?i)(?:rssi|signal)[^0-9-]*(-?[0-9]+)`)
var imsiPattern = regexp.MustCompile(`(?m)^IMSI is [0-9]{6,20}$`)
var phoneNumberPattern = regexp.MustCompile(`^[0-9]{3,20}$`)
var callStatePattern = regexp.MustCompile(`(?im)^call state ([A-Z_]+)$`)

func callIDForPeer(peer string) string { return "module:qdc507:" + peer }

func parseCallState(output string) (string, bool) {
	matches := callStatePattern.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return "", false
	}
	state := strings.ToUpper(matches[len(matches)-1][1])
	switch state {
	case "ORIGINATION", "CC_IN_PROGRESS", "ALERTING":
		return "connecting", true
	case "CONVERSATION":
		return "active", true
	case "DISCONNECTING", "END":
		return "ended", true
	default:
		return "", false
	}
}

func parseSignal(output string) int {
	match := signalPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return 0
	}
	value, err := strconv.Atoi(match[1])
	if err != nil || value < 0 {
		return 0
	}
	if value > 5 {
		return 5
	}
	return value
}

var atPortCandidates = []string{
	"/dev/at_usb0",
	"/dev/ttyUSB2",
	"/dev/ttyUSB1",
	"/dev/ttyUSB0",
	"/dev/ttyUSB3",
	"/dev/at_mhi0",
	"/dev/ttyACM0",
}

func sendSMSViaAT(ctx context.Context, pdu string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	pduLen := len(pdu)/2 - 1
	if pduLen <= 0 {
		return errors.New("invalid PDU length for AT")
	}
	// First try host-assisted AT via ECM (proven to work with the same PDU).
	// The host forwarder listens on the host's ECM address (192.168.225.22:8789).
	// This avoids the blocking /dev/at_usb0 issue inside the module.
	if err := sendSMSViaHostForwarder(ctx, pdu, timeout); err == nil {
		return nil
	} else {
		// Keep the error for fallback; if host forwarder is not running, try local TTY.
		// We do not return yet – fall through to local ports.
		_ = err
	}
	var lastErr error
	for _, port := range atPortCandidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(port); err != nil {
			continue
		}
		if err := tryATPort(ctx, port, pdu, pduLen, timeout); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return fmt.Errorf("AT fallback failed on all ports: %w", lastErr)
	}
	return errors.New("no AT port available for SMS fallback")
}

func sendSMSViaHostForwarder(ctx context.Context, pdu string, timeout time.Duration) error {
	candidates := []string{
		"http://192.168.225.22:8789/at-sms",
		"http://192.168.225.2:8789/at-sms",
		"http://192.168.225.10:8789/at-sms",
	}
	if v := os.Getenv("CELLBRIDGE_HOST_AT_URL"); v != "" {
		candidates = append([]string{v}, candidates...)
	}
	body, _ := json.Marshal(map[string]string{"pdu": pdu})
	for _, url := range candidates {
		reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, "POST", url, bytes.NewReader(body))
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{}
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		var out struct {
			OK  bool   `json:"ok"`
			Raw string `json:"raw"`
			Err string `json:"error"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			continue
		}
		if out.OK {
			return nil
		}
	}
	return errors.New("host AT forwarder not reachable")
}

func tryATPort(ctx context.Context, port, pdu string, pduLen int, timeout time.Duration) error {
	file, err := os.OpenFile(port, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	atCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Ensure a blocked Read is unblocked when context expires.
	go func() {
		<-atCtx.Done()
		_ = file.Close()
	}()
	// Helper to send line and collect until marker.
	sendCollect := func(cmd string, expect string, collectTimeout time.Duration) (string, error) {
		if cmd != "" {
			if _, err := file.Write([]byte(cmd)); err != nil {
				return "", err
			}
		}
		collected := ""
		deadline := time.Now().Add(collectTimeout)
		buf := make([]byte, 4096)
		for time.Now().Before(deadline) {
			if atCtx.Err() != nil {
				return collected, atCtx.Err()
			}
			// Use a per-read goroutine so we can timeout the blocking Read.
			ch := make(chan int, 1)
			errCh := make(chan error, 1)
			go func() {
				n, err := file.Read(buf)
				ch <- n
				errCh <- err
			}()
			select {
			case n := <-ch:
				err := <-errCh
				if err != nil && n == 0 {
					// Read error (e.g. file closed) – treat as no data.
					time.Sleep(100 * time.Millisecond)
					continue
				}
				if n > 0 {
					collected += string(buf[:n])
					if expect != "" && strings.Contains(collected, expect) {
						return collected, nil
					}
					if strings.Contains(collected, "OK") || strings.Contains(collected, "ERROR") || strings.Contains(collected, ">") {
						if expect == ">" && strings.Contains(collected, ">") {
							return collected, nil
						}
						if expect != ">" && strings.Contains(collected, "OK") {
							return collected, nil
						}
						if strings.Contains(collected, "ERROR") {
							return collected, nil
						}
					}
				}
			case <-time.After(400 * time.Millisecond):
				if collected != "" && (strings.Contains(collected, "OK") || strings.Contains(collected, ">") || strings.Contains(collected, "ERROR")) {
					return collected, nil
				}
			case <-atCtx.Done():
				return collected, atCtx.Err()
			}
			if time.Now().After(deadline) {
				break
			}
		}
		return collected, nil
	}
	// Abort any stale PDU input mode.
	_, _ = file.Write([]byte{0x1B})
	time.Sleep(300 * time.Millisecond)
	// Drain
	_, _ = sendCollect("", "", 400*time.Millisecond)
	// Basic AT sanity.
	out, _ := sendCollect("AT\r", "OK", 2*time.Second)
	if !strings.Contains(out, "OK") {
		return fmt.Errorf("AT not ready on %s: %q", port, out)
	}
	out, _ = sendCollect("AT+CMGF=0\r", "OK", 2*time.Second)
	if !strings.Contains(out, "OK") {
		return fmt.Errorf("CMGF failed on %s: %q", port, out)
	}
	cmd := fmt.Sprintf("AT+CMGS=%d\r", pduLen)
	out, _ = sendCollect(cmd, ">", 4*time.Second)
	if !strings.Contains(out, ">") {
		_, _ = file.Write([]byte{0x1B})
		return fmt.Errorf("no > prompt on %s: %q", port, out)
	}
	payload := pdu + string([]byte{0x1A})
	out, _ = sendCollect(payload, "OK", 10*time.Second)
	if strings.Contains(out, "+CMGS:") && strings.Contains(out, "OK") {
		return nil
	}
	if strings.Contains(out, "+CMS ERROR") {
		_, _ = file.Write([]byte{0x1B})
		return fmt.Errorf("CMS ERROR on %s: %q", port, out)
	}
	if strings.Contains(out, "OK") {
		return nil
	}
	_, _ = file.Write([]byte{0x1B})
	return fmt.Errorf("AT SMS no final OK on %s: %q", port, out)
}

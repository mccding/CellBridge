package at

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrCommandFailed = errors.New("AT command failed")
var ErrSubmissionResultUnknown = errors.New("SMS submission result is unknown")

// Parser accepts fragmented serial reads and emits complete AT lines. It also
// normalizes CRLF/CR line endings, which makes URC and response tests portable.
type Parser struct {
	buffer []byte
}

func (p *Parser) Feed(chunk []byte) []string {
	p.buffer = append(p.buffer, chunk...)
	var lines []string
	for {
		i := bytes.IndexByte(p.buffer, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(strings.TrimSuffix(string(p.buffer[:i]), "\r"))
		p.buffer = p.buffer[i+1:]
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func IsFinal(line string) bool {
	switch {
	case line == "OK", line == "ERROR":
		return true
	case strings.HasPrefix(line, "+CME ERROR"), strings.HasPrefix(line, "+CMS ERROR"):
		return true
	default:
		return false
	}
}

type Client struct {
	port       io.ReadWriteCloser
	mu         sync.Mutex
	ioCh       chan struct{}
	pollParser Parser
	lineHandle func(string)
	// DryRun makes SMS submissions use AT+CMGW (store-only) instead of
	// AT+CMGS (network send). Costs nothing and never delivers; used for
	// validation so we never burn the user's SMS balance
	// (2026-09-06: three real SMS were accidentally sent once).
	DryRun bool
}

func NewClient(port io.ReadWriteCloser) *Client {
	return &Client{port: port, ioCh: make(chan struct{}, 1)}
}

// acquireIO takes the serial-port lock with caller-visible cancellation.
// A plain mutex here lets a long SMS poll hold the port beyond a dial's
// deadline — the dial then only fails AFTER the lock frees (observed
// 2026-09-06: reboot + unresponsive module → SMS poll pinned port 15s
// → dial blocked 26s total → 500). With a buffered-channel lock the
// dial returns promptly on its own deadline instead.
func (c *Client) acquireIO(ctx context.Context) error {
	select {
	case c.ioCh <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) releaseIO() { <-c.ioCh }

func (c *Client) SetLineHandler(handler func(string)) {
	c.mu.Lock()
	c.lineHandle = handler
	c.mu.Unlock()
}

func (c *Client) SetDryRun(dryRun bool) {
	c.mu.Lock()
	c.DryRun = dryRun
	c.mu.Unlock()
}

func (c *Client) Close() error { return c.port.Close() }

func (c *Client) Exchange(ctx context.Context, command string) ([]string, error) {
	if err := c.acquireIO(ctx); err != nil {
		return nil, err
	}
	defer c.releaseIO()
	c.mu.Lock()
	response, unsolicited, err := c.exchangeLocked(ctx, command)
	handler := c.lineHandle
	c.mu.Unlock()
	dispatchUnsolicited(handler, unsolicited)
	return response, err
}

func (c *Client) exchangeLocked(ctx context.Context, command string) ([]string, []string, error) {
	if _, err := io.WriteString(c.port, command+"\r"); err != nil {
		return nil, nil, fmt.Errorf("write AT command: %w", err)
	}

	parser := Parser{}
	buf := make([]byte, 256)
	var response []string
	var unsolicited []string
	for {
		select {
		case <-ctx.Done():
			return nil, unsolicited, ctx.Err()
		default:
		}
		n, err := readWithContext(ctx, c.port, buf)
		if n > 0 {
			for _, line := range parser.Feed(buf[:n]) {
				if strings.EqualFold(line, command) {
					continue
				}
				if isUnsolicited(line) {
					unsolicited = append(unsolicited, line)
					continue
				}
				response = append(response, line)
				if IsFinal(line) {
					if line != "OK" {
						return response, unsolicited, fmt.Errorf("%w: %s", ErrCommandFailed, line)
					}
					return response, unsolicited, nil
				}
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
				return nil, unsolicited, context.DeadlineExceeded
			}
			return nil, unsolicited, fmt.Errorf("read AT response: %w", err)
		}
	}
}

// SendPDU handles the text prompt used by AT+CMGS in PDU mode. Keeping this
// sequence inside Client makes it atomic with respect to status and call
// commands issued by other workers.
func (c *Client) SendPDU(ctx context.Context, tpduLength int, pdu string) ([]string, error) {
	if err := c.acquireIO(ctx); err != nil {
		return nil, err
	}
	defer c.releaseIO()
	c.mu.Lock()
	if tpduLength < 1 || strings.TrimSpace(pdu) == "" {
		c.mu.Unlock()
		return nil, fmt.Errorf("invalid SMS PDU submission")
	}
	cmd := "AT+CMGS"
	if c.DryRun {
		cmd = "AT+CMGW"
	}
	if _, err := io.WriteString(c.port, fmt.Sprintf("%s=%d\r", cmd, tpduLength)); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("write %s: %w", cmd, err)
	}
	_, unsolicited, err := c.readUntil(ctx, true)
	if err != nil {
		handler := c.lineHandle
		c.mu.Unlock()
		dispatchUnsolicited(handler, unsolicited)
		return nil, err
	}
	if _, err := io.WriteString(c.port, strings.TrimSpace(pdu)+"\x1a"); err != nil {
		handler := c.lineHandle
		c.mu.Unlock()
		dispatchUnsolicited(handler, unsolicited)
		return nil, fmt.Errorf("write SMS PDU: %w", err)
	}
	response, trailing, err := c.readUntil(ctx, false)
	unsolicited = append(unsolicited, trailing...)
	handler := c.lineHandle
	c.mu.Unlock()
	dispatchUnsolicited(handler, unsolicited)
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrCommandFailed)) {
		return response, fmt.Errorf("%w: %v", ErrSubmissionResultUnknown, err)
	}
	return response, err
}

func (c *Client) SendTextSMS(ctx context.Context, destination, body string) ([]string, error) {
	if err := c.acquireIO(ctx); err != nil {
		return nil, err
	}
	defer c.releaseIO()
	c.mu.Lock()
	if strings.TrimSpace(destination) == "" || strings.TrimSpace(body) == "" {
		c.mu.Unlock()
		return nil, fmt.Errorf("invalid SMS text submission")
	}
	cmd := "AT+CMGS"
	if c.DryRun {
		cmd = "AT+CMGW"
	}
	if _, err := io.WriteString(c.port, fmt.Sprintf("%s=\"%s\"\r", cmd, destination)); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("write %s text: %w", cmd, err)
	}
	_, unsolicited, err := c.readUntil(ctx, true)
	if err != nil {
		handler := c.lineHandle
		c.mu.Unlock()
		dispatchUnsolicited(handler, unsolicited)
		return nil, err
	}
	if _, err := io.WriteString(c.port, body+"\x1a"); err != nil {
		handler := c.lineHandle
		c.mu.Unlock()
		dispatchUnsolicited(handler, unsolicited)
		return nil, fmt.Errorf("write SMS body: %w", err)
	}
	response, trailing, err := c.readUntil(ctx, false)
	unsolicited = append(unsolicited, trailing...)
	handler := c.lineHandle
	c.mu.Unlock()
	dispatchUnsolicited(handler, unsolicited)
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrCommandFailed)) {
		return response, fmt.Errorf("%w: %v", ErrSubmissionResultUnknown, err)
	}
	return response, err
}

// Run drains unsolicited modem lines while the port is idle. A command
// exchange and this idle reader share the port lock (ioCh), so they
// never read the same byte concurrently. The idle path performs one
// non-blocking read at a time; if a command holds the lock it simply
// skips this poll (lock is contended by design — never block URC reads
// behind a command deadline).
func (c *Client) Run(ctx context.Context) error {
	buf := make([]byte, 256)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var n int
		select {
		case c.ioCh <- struct{}{}:
			n, _ = readAvailable(c.port, buf)
			<-c.ioCh
		default:
			// A command exchange owns the port; skip this poll.
		}
		var lines []string
		if n > 0 {
			c.mu.Lock()
			lines = c.pollParser.Feed(buf[:n])
			c.mu.Unlock()
		}
		c.mu.Lock()
		handler := c.lineHandle
		c.mu.Unlock()
		for _, line := range lines {
			if handler != nil {
				handler(line)
			}
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		}
	}
}

func readAvailable(port io.ReadWriteCloser, buffer []byte) (int, error) {
	if _, ok := port.(*os.File); ok {
		return readOnce(port, buffer)
	}
	return port.Read(buffer)
}

func (c *Client) readUntil(ctx context.Context, prompt bool) ([]string, []string, error) {
	parser := Parser{}
	buf := make([]byte, 256)
	var response []string
	var unsolicited []string
	for {
		select {
		case <-ctx.Done():
			return nil, unsolicited, ctx.Err()
		default:
		}
		n, err := readWithContext(ctx, c.port, buf)
		if n > 0 {
			chunk := buf[:n]
			if prompt && bytes.Contains(chunk, []byte(">")) {
				return response, unsolicited, nil
			}
			for _, line := range parser.Feed(chunk) {
				if isUnsolicited(line) {
					unsolicited = append(unsolicited, line)
					continue
				}
				response = append(response, line)
				if IsFinal(line) {
					if line != "OK" {
						return response, unsolicited, fmt.Errorf("%w: %s", ErrCommandFailed, line)
					}
					return response, unsolicited, nil
				}
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
				return nil, unsolicited, context.DeadlineExceeded
			}
			return nil, unsolicited, fmt.Errorf("read AT response: %w", err)
		}
	}
}

func isUnsolicited(line string) bool {
	line = strings.TrimSpace(line)
	return line == "RING" || strings.Contains(line, "NO CARRIER") || strings.Contains(line, "BUSY") || strings.Contains(line, "NO ANSWER")
}

func dispatchUnsolicited(handler func(string), lines []string) {
	if handler == nil {
		return
	}
	for _, line := range lines {
		handler(line)
	}
}

type readResult struct {
	n   int
	err error
}

// readWithContext works with ordinary os.File-backed TTYs, whose Read method
// often does not implement SetReadDeadline. On cancellation, closing the fd
// is the portable way to wake the blocked kernel read. The caller is already
// abandoning that serial client, so the close is intentional.
func readWithContext(ctx context.Context, port io.ReadWriteCloser, buffer []byte) (int, error) {
	for {
		result := make(chan readResult, 1)
		go func() {
			n, err := readOnce(port, buffer)
			result <- readResult{n: n, err: err}
		}()
		select {
		case read := <-result:
			if !isRetryableReadError(read.err) {
				return read.n, read.err
			}
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				closeReadPort(port)
				return 0, ctx.Err()
			}
		case <-ctx.Done():
			closeReadPort(port)
			return 0, ctx.Err()
		}
	}
}

func readOnce(port io.ReadWriteCloser, buffer []byte) (int, error) {
	if file, ok := port.(*os.File); ok {
		// Bypass os.File's internal poller for character devices. The fd is
		// explicitly non-blocking, so this returns EAGAIN instead of leaving
		// a goroutine stuck in the kernel when a USB interface is silent.
		return syscall.Read(int(file.Fd()), buffer)
	}
	return port.Read(buffer)
}

func closeReadPort(port io.ReadWriteCloser) {
	// Closing an os.File while its internal poller owns a character-device
	// read can itself wait for that read. OpenSerial uses direct non-blocking
	// syscall reads, so its caller can close the file after Exchange returns.
	if _, ok := port.(*os.File); ok {
		return
	}
	_ = port.Close()
}

func isRetryableReadError(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)
}

// OpenSerial configures a plain serial device. The exact termios call is
// intentionally kept at the edge; modem logic only depends on Client.
func OpenSerial(ctx context.Context, path string, baud int) (*Client, error) {
	// Some USB serial interfaces block during open while the modem driver is
	// being re-enumerated. O_NONBLOCK makes automatic discovery and all later
	// reads bounded; readWithContext handles EAGAIN without busy spinning.
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open serial port %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock serial port %s: %w", path, err)
	}
	if err := syscall.SetNonblock(int(file.Fd()), true); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("configure nonblocking mode for serial port %s: %w", path, err)
	}
	if err := configureSerial(ctx, path, baud); err != nil {
		_ = file.Close()
		return nil, err
	}
	return NewClient(file), nil
}

func configureSerial(ctx context.Context, path string, baud int) error {
	if runtime.GOOS == "linux" {
		return configureSerialNative(ctx, path, baud)
	}
	args := []string{"raw", "-echo", fmt.Sprint(baud)}
	if runtime.GOOS == "darwin" {
		args = append([]string{"-f", path}, args...)
	} else {
		args = append([]string{"-F", path}, args...)
	}
	command := execCommandContext(ctx, "stty", args...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("configure serial port: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

var execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

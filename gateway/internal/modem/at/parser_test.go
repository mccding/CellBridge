package at

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

func TestParserFragmentedReads(t *testing.T) {
	p := Parser{}
	var got []string
	got = append(got, p.Feed([]byte("ATI\r"))...)
	got = append(got, p.Feed([]byte("\nQuectel\r\nO"))...)
	got = append(got, p.Feed([]byte("K\r\n"))...)
	want := []string{"ATI", "Quectel", "OK"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestIsFinal(t *testing.T) {
	for _, line := range []string{"OK", "ERROR", "+CME ERROR: 3", "+CMS ERROR: 500"} {
		if !IsFinal(line) {
			t.Errorf("%q should be final", line)
		}
	}
	if IsFinal("+CSQ: 20,99") {
		t.Error("intermediate response marked final")
	}
}

type blockingPort struct {
	closed chan struct{}
}

func newBlockingPort() *blockingPort { return &blockingPort{closed: make(chan struct{})} }

func (p *blockingPort) Write(data []byte) (int, error) { return len(data), nil }

func (p *blockingPort) Read([]byte) (int, error) {
	<-p.closed
	return 0, io.ErrClosedPipe
}

func (p *blockingPort) Close() error {
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
	return nil
}

func TestExchangeCancelsBlockedSerialRead(t *testing.T) {
	port := newBlockingPort()
	client := NewClient(port)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := client.Exchange(ctx, "AT")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exchange error = %v, want deadline exceeded", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("blocked exchange did not cancel promptly")
	}
}

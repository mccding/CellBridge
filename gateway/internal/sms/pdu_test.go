package sms

import (
	"strings"
	"testing"
)

func TestGSM7SubmitSegment(t *testing.T) {
	segments, err := EncodeSubmitSegments("+8613800138000", "hello {}", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || segments[0].Encoding != EncodingGSM7 {
		t.Fatalf("unexpected segments: %#v", segments)
	}
	if !strings.HasPrefix(segments[0].PDU, "0001000D91") {
		t.Fatalf("unexpected PDU header: %s", segments[0].PDU)
	}
}

func TestUCS2ChineseSubmitSegment(t *testing.T) {
	segments, err := EncodeSubmitSegments("10086", "测试短信", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || segments[0].Encoding != EncodingUCS2 {
		t.Fatalf("unexpected segments: %#v", segments)
	}
	if !strings.Contains(segments[0].PDU, "0008") {
		t.Fatalf("missing UCS-2 DCS: %s", segments[0].PDU)
	}
}

func TestMultipartUsesConcatenationHeader(t *testing.T) {
	segments, err := EncodeSubmitSegments("+8613800138000", strings.Repeat("a", 200), 0x42)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].Total != 2 || segments[1].PartNo != 2 {
		t.Fatalf("unexpected multipart result: %#v", segments)
	}
	// UDHI + 05 00 03 42 02 01 appears after the submit header.
	if !strings.Contains(segments[0].PDU, "050003420201") {
		t.Fatalf("missing concatenation UDH: %s", segments[0].PDU)
	}
}

func TestDecodeUCS2SubmitRoundTrip(t *testing.T) {
	segments, err := EncodeSubmitSegments("+8613800138000", "你好 CellBridge", 3)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePDU(segments[0].PDU)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Body != "你好 CellBridge" || decoded.Encoding != EncodingUCS2 || decoded.Sender != "+8613800138000" {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestDecodeGSM7SubmitRoundTrip(t *testing.T) {
	segments, err := EncodeSubmitSegments("10086", "hello", 3)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePDU(segments[0].PDU)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Body != "hello" || decoded.Encoding != EncodingGSM7 || decoded.Sender != "10086" {
		t.Fatalf("decoded = %#v", decoded)
	}
}

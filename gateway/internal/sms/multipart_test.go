package sms

import (
	"testing"
	"time"
)

func TestMultipartOutOfOrderAndDuplicate(t *testing.T) {
	assembler := NewMultipartAssembler()
	now := time.Unix(100, 0)
	first, err := assembler.Add(Segment{Sender: "+86138", ConcatRef: 4, PartNo: 2, TotalParts: 3, Body: "世界", ReceivedAt: now.Add(time.Second)})
	if err != nil || first != nil {
		t.Fatalf("first add = %#v, %v", first, err)
	}
	duplicate, err := assembler.Add(Segment{Sender: "+86138", ConcatRef: 4, PartNo: 2, TotalParts: 3, Body: "错误", ReceivedAt: now.Add(2 * time.Second)})
	if err != nil || duplicate != nil {
		t.Fatalf("duplicate add = %#v, %v", duplicate, err)
	}
	if complete, err := assembler.Add(Segment{Sender: "+86138", ConcatRef: 4, PartNo: 1, TotalParts: 3, Body: "你好", ReceivedAt: now}); err != nil || complete != nil {
		t.Fatalf("second add = %#v, %v", complete, err)
	}
	complete, err := assembler.Add(Segment{Sender: "+86138", ConcatRef: 4, PartNo: 3, TotalParts: 3, Body: "!", ReceivedAt: now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if complete == nil || complete.Body != "你好世界!" || len(complete.Segments) != 3 {
		t.Fatalf("complete = %#v", complete)
	}
}

func TestMultipartPrune(t *testing.T) {
	assembler := NewMultipartAssembler()
	old := time.Now().Add(-MultipartRetention - time.Minute)
	_, err := assembler.Add(Segment{Sender: "+86138", ConcatRef: 8, PartNo: 1, TotalParts: 2, Body: "partial", ReceivedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	expired := assembler.Prune(time.Now())
	if len(expired) != 1 || expired[0].Body != "partial" {
		t.Fatalf("expired = %#v", expired)
	}
}

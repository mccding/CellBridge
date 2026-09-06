package events

import "testing"

func TestJournalCursorAndEviction(t *testing.T) {
	j := NewJournal(2)
	j.Append("one", nil, 1)
	j.Append("two", nil, 2)
	j.Append("three", nil, 3)
	if _, ok := j.Since(0); ok {
		t.Fatal("expected an evicted cursor to fail")
	}
	values, ok := j.Since(1)
	if !ok || len(values) != 2 || values[0].Seq != 2 || values[1].Seq != 3 {
		t.Fatalf("unexpected journal slice: %#v %v", values, ok)
	}
}

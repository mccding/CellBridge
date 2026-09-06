package sms

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const MultipartRetention = 24 * time.Hour

type Segment struct {
	Sender      string
	Destination string
	ConcatRef   uint8
	PartNo      int
	TotalParts  int
	Body        string
	Encoding    Encoding
	RawPDU      []byte
	ReceivedAt  time.Time
}

type LogicalMessage struct {
	Sender     string
	Body       string
	Encoding   Encoding
	ConcatRef  uint8
	TotalParts int
	Segments   []Segment
	ReceivedAt time.Time
}

type multipartKey struct {
	sender      string
	destination string
	concatRef   uint8
	totalParts  int
}

type MultipartAssembler struct {
	mu      sync.Mutex
	pending map[multipartKey]map[int]Segment
}

func NewMultipartAssembler() *MultipartAssembler {
	return &MultipartAssembler{pending: make(map[multipartKey]map[int]Segment)}
}

// Add is safe for concurrent modem workers. A nil result means that more
// segments are needed or the segment was a duplicate.
func (a *MultipartAssembler) Add(segment Segment) (*LogicalMessage, error) {
	if segment.PartNo < 1 || segment.TotalParts < 2 || segment.PartNo > segment.TotalParts {
		return nil, fmt.Errorf("invalid multipart segment %d/%d", segment.PartNo, segment.TotalParts)
	}
	if segment.ReceivedAt.IsZero() {
		segment.ReceivedAt = time.Now()
	}
	key := multipartKey{sender: segment.Sender, destination: segment.Destination, concatRef: segment.ConcatRef, totalParts: segment.TotalParts}
	a.mu.Lock()
	defer a.mu.Unlock()
	parts := a.pending[key]
	if parts == nil {
		parts = make(map[int]Segment, segment.TotalParts)
		a.pending[key] = parts
	}
	if _, exists := parts[segment.PartNo]; exists {
		return nil, nil
	}
	parts[segment.PartNo] = segment
	if len(parts) != segment.TotalParts {
		return nil, nil
	}
	ordered := make([]Segment, 0, segment.TotalParts)
	var body strings.Builder
	var receivedAt time.Time
	for partNo := 1; partNo <= segment.TotalParts; partNo++ {
		part, exists := parts[partNo]
		if !exists {
			return nil, nil
		}
		ordered = append(ordered, part)
		body.WriteString(part.Body)
		if receivedAt.IsZero() || part.ReceivedAt.Before(receivedAt) {
			receivedAt = part.ReceivedAt
		}
	}
	delete(a.pending, key)
	return &LogicalMessage{
		Sender:     segment.Sender,
		Body:       body.String(),
		Encoding:   segment.Encoding,
		ConcatRef:  segment.ConcatRef,
		TotalParts: segment.TotalParts,
		Segments:   ordered,
		ReceivedAt: receivedAt,
	}, nil
}

// Prune removes incomplete multipart messages older than retention and
// returns their segments for optional diagnostic persistence.
func (a *MultipartAssembler) Prune(now time.Time) []Segment {
	cutoff := now.Add(-MultipartRetention)
	a.mu.Lock()
	defer a.mu.Unlock()
	var expired []Segment
	for key, parts := range a.pending {
		latest := time.Time{}
		for _, part := range parts {
			if part.ReceivedAt.After(latest) {
				latest = part.ReceivedAt
			}
		}
		if !latest.IsZero() && latest.Before(cutoff) {
			for _, part := range parts {
				expired = append(expired, part)
			}
			delete(a.pending, key)
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].ReceivedAt.Before(expired[j].ReceivedAt) })
	return expired
}

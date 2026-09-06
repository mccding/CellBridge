package sms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/db"
	"github.com/cellbridge/cellbridge/gateway/internal/id"
	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

type Modem interface {
	SendSMS(context.Context, string, modem.SMSPayload) (modem.SMSID, error)
}

type InboxModem interface {
	ListSMS(context.Context, modem.SMSCursor) ([]modem.RawSMS, modem.SMSCursor, error)
	DeleteSMS(context.Context, string) error
}

type Engine struct {
	Database  *db.DB
	Modem     Modem
	Assembler *MultipartAssembler
	Now       func() time.Time
}

func NewEngine(database *db.DB, modemClient Modem) *Engine {
	return &Engine{Database: database, Modem: modemClient, Assembler: NewMultipartAssembler(), Now: time.Now}
}

// Ingest decodes one modem PDU. A nil result means a multipart message is
// still waiting for another segment or the PDU was already seen.
func (e *Engine) Ingest(ctx context.Context, raw modem.RawSMS) (*db.Message, error) {
	pduHash := sha256.Sum256(raw.RawPDU)
	hash := hex.EncodeToString(pduHash[:])
	exists, err := e.Database.SMSSegmentExistsByPDUHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, nil
	}
	decoded, err := DecodePDU(hex.EncodeToString(raw.RawPDU))
	if err != nil {
		return nil, err
	}
	receivedAt := e.Now()
	segmentID, err := id.New("seg_", 16)
	if err != nil {
		return nil, err
	}
	if err := e.Database.InsertSMSSegment(ctx, db.SMSSegment{
		ID: segmentID, Sender: decoded.Sender, ConcatRef: decoded.ConcatRef, PartNo: decoded.PartNo,
		TotalParts: decoded.TotalParts, RawPDU: raw.RawPDU, PDUHash: hash, ReceivedAt: receivedAt,
	}); err != nil {
		return nil, err
	}
	body := decoded.Body
	encoding := decoded.Encoding
	var completed *LogicalMessage
	if decoded.TotalParts > 1 {
		pending, listErr := e.Database.ListPendingSMSSegments(ctx, decoded.Sender, decoded.ConcatRef, decoded.TotalParts)
		if listErr != nil {
			return nil, listErr
		}
		for _, stored := range pending {
			storedDecoded, decodeErr := DecodePDU(hex.EncodeToString(stored.RawPDU))
			if decodeErr != nil {
				continue
			}
			completed, err = e.Assembler.Add(Segment{Sender: storedDecoded.Sender, ConcatRef: storedDecoded.ConcatRef, PartNo: storedDecoded.PartNo, TotalParts: storedDecoded.TotalParts, Body: storedDecoded.Body, Encoding: storedDecoded.Encoding, RawPDU: stored.RawPDU, ReceivedAt: stored.ReceivedAt})
			if err != nil {
				return nil, err
			}
			if completed != nil {
				break
			}
		}
		if completed == nil {
			return nil, nil
		}
		body = completed.Body
		encoding = completed.Encoding
	}
	messageID, err := id.New("msg_", 16)
	if err != nil {
		return nil, err
	}
	threadKey := decoded.Sender
	message := db.Message{ID: messageID, ThreadKey: threadKey, Direction: "inbound", Peer: decoded.Sender, Body: body, Encoding: string(encoding), Status: "sent", CreatedAt: receivedAt, ModemStorage: raw.ModemStorage, ModemIndex: &raw.ModemIndex, PDUHash: hash}
	sequence, err := e.Database.InsertMessage(ctx, message)
	if err != nil {
		return nil, err
	}
	if completed == nil {
		if err := e.Database.LinkSMSSegment(ctx, hash, messageID); err != nil {
			return nil, err
		}
	} else {
		for _, segment := range completed.Segments {
			segmentHash := sha256.Sum256(segment.RawPDU)
			if err := e.Database.LinkSMSSegment(ctx, hex.EncodeToString(segmentHash[:]), messageID); err != nil {
				return nil, err
			}
		}
	}
	message.SyncSeq = sequence
	return &message, nil
}

func (e *Engine) Send(ctx context.Context, to, body string) (*db.Message, error) {
	if e.Modem == nil {
		return nil, fmt.Errorf("SMS modem is unavailable")
	}
	concatRef := uint8(e.Now().UnixNano())
	segments, err := EncodeSubmitSegments(to, body, concatRef)
	if err != nil {
		return nil, err
	}
	messageID, err := id.New("msg_", 16)
	if err != nil {
		return nil, err
	}
	now := e.Now()
	message := db.Message{ID: messageID, ThreadKey: to, Direction: "outbound", Peer: to, Body: body, Encoding: string(segments[0].Encoding), Status: "queued", CreatedAt: now}
	sequence, err := e.Database.InsertMessage(ctx, message)
	if err != nil {
		return nil, err
	}
	message.SyncSeq = sequence

	for _, segment := range segments {
		_, sendErr := e.Modem.SendSMS(ctx, to, modem.SMSPayload{Body: segment.Body, Encoding: string(segment.Encoding), PDU: segment.PDU, PartNo: segment.PartNo, Total: segment.Total})
		if sendErr != nil {
			if errors.Is(sendErr, modem.ErrSMSSubmissionUnknown) {
				message.Status = "submitted"
				if updatedSeq, updateErr := e.Database.UpdateMessageStatus(ctx, message.ID, message.Status); updateErr != nil {
					return &message, fmt.Errorf("record uncertain SMS submission: %w", updateErr)
				} else {
					message.SyncSeq = updatedSeq
				}
				slog.Warn("SMS submission result unknown after modem accepted PDU", "message_id", message.ID, "part", segment.PartNo, "total_parts", segment.Total, "error", sendErr)
				return &message, nil
			}
			message.Status = "failed"
			if updatedSeq, updateErr := e.Database.UpdateMessageStatus(ctx, message.ID, message.Status); updateErr != nil {
				return &message, fmt.Errorf("record failed SMS submission: %w", updateErr)
			} else {
				message.SyncSeq = updatedSeq
			}
			slog.Warn("SMS PDU submission failed", "message_id", message.ID, "part", segment.PartNo, "total_parts", segment.Total, "error", sendErr)
			return &message, fmt.Errorf("SMS submit failed: %w", sendErr)
		}
		message.Status = "submitted"
		if updatedSeq, updateErr := e.Database.UpdateMessageStatus(ctx, message.ID, message.Status); updateErr != nil {
			return &message, fmt.Errorf("record SMS submission: %w", updateErr)
		} else {
			message.SyncSeq = updatedSeq
		}
	}
	message.Status = "sent"
	if updatedSeq, updateErr := e.Database.UpdateMessageStatus(ctx, message.ID, message.Status); updateErr != nil {
		return &message, fmt.Errorf("record SMS sent status: %w", updateErr)
	} else {
		message.SyncSeq = updatedSeq
	}
	return &message, nil
}

// Run polls the modem inbox, consumes complete messages, and deletes only
// successfully decoded/inserted storage entries. A crash before deletion is
// safe because PDU hashes make the next poll idempotent.
func (e *Engine) Run(ctx context.Context, interval time.Duration, onMessage func(*db.Message)) error {
	inbox, ok := e.Modem.(InboxModem)
	if !ok {
		return fmt.Errorf("modem does not support SMS inbox polling")
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	var cursor modem.SMSCursor
	poll := func() error {
		e.Assembler.Prune(e.Now())
		_ = e.Database.PrunePendingSMSSegments(ctx, e.Now().Add(-MultipartRetention))
		messages, next, err := inbox.ListSMS(ctx, cursor)
		if err != nil {
			return err
		}
		for _, raw := range messages {
			message, ingestErr := e.Ingest(ctx, raw)
			if ingestErr != nil {
				continue
			}
			if message != nil && onMessage != nil {
				onMessage(message)
			}
			if raw.ModemIndex > 0 {
				_ = inbox.DeleteSMS(ctx, fmt.Sprint(raw.ModemIndex))
			}
		}
		cursor = next
		return nil
	}
	if err := poll(); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = poll()
		}
	}
}

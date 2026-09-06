package modem

import (
	"context"
	"errors"
)

type CallID string
type SMSID string
type SMSCursor string

// ErrSMSSubmissionUnknown means the modem accepted the PDU write but the
// final result was lost (for example during a serial timeout). The carrier
// may already have received the message, so callers must not report a plain
// failed send or blindly retry it.
var ErrSMSSubmissionUnknown = errors.New("SMS submission result is unknown")

// ErrActiveCall indicates that a new dial was rejected because the modem
// still owns a call. The API uses it to perform a bounded stale-call cleanup
// before allowing the next dial attempt.
var ErrActiveCall = errors.New("active modem call exists")

type ModemControl interface {
	Probe(context.Context) (Capabilities, error)
	Status(context.Context) (LineStatus, error)
	Dial(context.Context, string) (CallID, error)
	Answer(context.Context, CallID) error
	Hangup(context.Context, CallID) error
	SendDTMF(context.Context, CallID, rune) error
	ListSMS(context.Context, SMSCursor) ([]RawSMS, SMSCursor, error)
	SendSMS(context.Context, string, SMSPayload) (SMSID, error)
	DeleteSMS(context.Context, string) error
	Events() <-chan ModemEvent
	Close() error
}

type VoiceAudio interface {
	Probe(context.Context) (AudioCapabilities, error)
	Start(context.Context, CallID) error
	ReadPCM([]int16) (int, error)
	WritePCM([]int16) (int, error)
	Stop(context.Context) error
	Close() error
}

type Capabilities struct {
	Vendor              string                 `json:"vendor"`
	Model               string                 `json:"model"`
	USBVID              string                 `json:"usbVid"`
	USBPID              string                 `json:"usbPid"`
	Tier                string                 `json:"tier"`
	SMS                 bool                   `json:"sms"`
	Voice               bool                   `json:"voice"`
	DTMF                bool                   `json:"dtmf"`
	Audio               *AudioCapabilities     `json:"audio,omitempty"`
	Recording           *RecordingCapabilities `json:"recording,omitempty"`
	RequiresBootstrap   bool                   `json:"requiresBootstrap"`
	FirmwareFingerprint string                 `json:"firmwareFingerprint,omitempty"`
}

type RecordingCapabilities struct {
	Supported bool   `json:"supported"`
	Manual    bool   `json:"manual"`
	Auto      bool   `json:"auto"`
	Format    string `json:"format"`
}

type AudioCapabilities struct {
	Backend    string `json:"backend"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
}

type LineStatus struct {
	SIM            string  `json:"sim"`
	Operator       string  `json:"operator,omitempty"`
	Registration   string  `json:"registration"`
	Signal         Signal  `json:"signal"`
	CapabilityTier string  `json:"capabilityTier"`
	Voice          string  `json:"voice"`
	SMS            string  `json:"sms"`
	ActiveCallID   *CallID `json:"activeCallId"`
}

const (
	CapabilityUnknown          = "unknown"
	CapabilitySMSOnly          = "sms_only"
	CapabilityVoiceControlOnly = "voice_control_only"
	CapabilityFullVoice        = "full_voice"
)

type Signal struct {
	RSSI int `json:"rssi"`
	Bars int `json:"bars"`
}

type SMSPayload struct {
	Body     string `json:"body"`
	Encoding string `json:"encoding,omitempty"`
	PDU      string `json:"pdu,omitempty"`
	PartNo   int    `json:"partNo,omitempty"`
	Total    int    `json:"totalParts,omitempty"`
}

type RawSMS struct {
	ModemStorage    string
	ModemIndex      int
	Sender          string
	ServiceTimeUnix int64
	RawPDU          []byte
}

type ModemEvent struct {
	Kind   string
	CallID CallID
	Peer   string
	Raw    string
}

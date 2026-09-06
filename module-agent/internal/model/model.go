package model

type Identity struct {
	DeviceID        string `json:"deviceId"`
	DeviceFamily    string `json:"deviceFamily"`
	AgentVersion    string `json:"agentVersion"`
	ProtocolVersion int    `json:"protocolVersion"`
}

type Health struct {
	State        string `json:"state"`
	Control      string `json:"control"`
	SMS          string `json:"sms"`
	Call         string `json:"call"`
	VoiceRuntime string `json:"voiceRuntime"`
	Media        string `json:"media"`
}

type Line struct {
	LineID       string `json:"lineId"`
	SIM          string `json:"sim"`
	Carrier      string `json:"carrier,omitempty"`
	Registration string `json:"registration"`
	Signal       int    `json:"signal"`
	Voice        bool   `json:"voice"`
	SMS          bool   `json:"sms"`
	DTMF         bool   `json:"dtmf"`
}

type Call struct {
	ID        string `json:"id"`
	LineID    string `json:"lineId"`
	Peer      string `json:"peer"`
	Direction string `json:"direction"`
	State     string `json:"state"`
}

type Event struct {
	Seq     int64       `json:"seq"`
	Type    string      `json:"type"`
	Time    int64       `json:"time"`
	Payload interface{} `json:"payload"`
}

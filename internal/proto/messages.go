package proto

import (
	"encoding/json"
	"time"
)

type MessageType string

const (
	MsgHello     MessageType = "hello"
	MsgWelcome   MessageType = "welcome"
	MsgHeartbeat MessageType = "heartbeat"
	MsgError     MessageType = "error"
)

type Envelope struct {
	Type MessageType     `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Hello struct {
	WorkerID    string   `json:"worker_id"`
	Hostname    string   `json:"hostname"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	NumCPU      int      `json:"num_cpu"`
	GPUs        []string `json:"gpus,omitempty"`
	Version     string   `json:"version"`
	LoadedModel string   `json:"loaded_model,omitempty"`
}

type Welcome struct {
	AssignedID    string        `json:"assigned_id"`
	ServerVersion string        `json:"server_version"`
	HeartbeatEvery time.Duration `json:"heartbeat_every_ns"`
}

type Heartbeat struct {
	Idle      bool      `json:"idle"`
	IdleSince time.Time `json:"idle_since,omitempty"`
}

type ErrorMsg struct {
	Message string `json:"message"`
}

func Encode(t MessageType, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(Envelope{Type: t, Data: raw})
}

func Decode(b []byte) (Envelope, error) {
	var e Envelope
	err := json.Unmarshal(b, &e)
	return e, err
}

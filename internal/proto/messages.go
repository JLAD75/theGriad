package proto

import (
	"encoding/json"
	"time"
)

type MessageType string

const (
	MsgHello       MessageType = "hello"
	MsgWelcome     MessageType = "welcome"
	MsgHeartbeat   MessageType = "heartbeat"
	MsgError       MessageType = "error"
	MsgChatRequest MessageType = "chat_request"
	MsgChatChunk   MessageType = "chat_chunk"
	MsgChatDone    MessageType = "chat_done"
	MsgCancel      MessageType = "cancel"
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

// ChatRequest carries an OpenAI-style chat completion request from the
// orchestrator to a worker. Body is the raw JSON request body, passed
// through to the worker's local llama-server with stream forced to true.
type ChatRequest struct {
	RequestID string          `json:"request_id"`
	Body      json.RawMessage `json:"body"`
}

// ChatChunk is one SSE line forwarded from llama-server back to the
// orchestrator. Data includes the trailing newline so the orchestrator
// can re-emit it verbatim.
type ChatChunk struct {
	RequestID string `json:"request_id"`
	Data      string `json:"data"`
}

// ChatDone signals end of stream. Error is empty on success.
type ChatDone struct {
	RequestID string `json:"request_id"`
	Error     string `json:"error,omitempty"`
}

// Cancel asks the worker to abort an in-flight request.
type Cancel struct {
	RequestID string `json:"request_id"`
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

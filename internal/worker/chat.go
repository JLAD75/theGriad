package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/JLAD75/theGriad/internal/proto"
)

// handleChat executes a chat request against the local llama-server and
// streams the SSE response back to the orchestrator one line at a time.
//
// llama-server's /v1/chat/completions, when called with stream=true, emits
// Server-Sent Events. We forward each line of the response (including
// "data: ..." and the empty line separator) as a ChatChunk so the
// orchestrator can re-emit the SSE verbatim to its HTTP client.
func (s *session) handleChat(parent context.Context, req proto.ChatRequest) {
	if s.llamaURL == "" {
		s.send(proto.MsgChatDone, proto.ChatDone{
			RequestID: req.RequestID,
			Error:     "worker has no model loaded",
		})
		return
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s.registerInflight(req.RequestID, cancel)
	defer s.clearInflight(req.RequestID)

	body, err := forceStream(req.Body)
	if err != nil {
		s.send(proto.MsgChatDone, proto.ChatDone{
			RequestID: req.RequestID,
			Error:     fmt.Sprintf("invalid request body: %v", err),
		})
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.llamaURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.send(proto.MsgChatDone, proto.ChatDone{RequestID: req.RequestID, Error: err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	// No client-side timeout: generation can be long. Cancellation comes
	// from the parent context (Cancel msg or session shutdown).
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			s.send(proto.MsgChatDone, proto.ChatDone{RequestID: req.RequestID, Error: "cancelled"})
			return
		}
		s.send(proto.MsgChatDone, proto.ChatDone{RequestID: req.RequestID, Error: err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		s.send(proto.MsgChatDone, proto.ChatDone{
			RequestID: req.RequestID,
			Error:     fmt.Sprintf("llama-server %d: %s", resp.StatusCode, string(buf)),
		})
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		s.send(proto.MsgChatChunk, proto.ChatChunk{
			RequestID: req.RequestID,
			Data:      line,
		})
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		s.send(proto.MsgChatDone, proto.ChatDone{RequestID: req.RequestID, Error: err.Error()})
		return
	}

	doneErr := ""
	if ctx.Err() != nil {
		doneErr = "cancelled"
	}
	s.send(proto.MsgChatDone, proto.ChatDone{RequestID: req.RequestID, Error: doneErr})
}

// forceStream re-encodes the request body with stream=true, regardless of
// what the caller sent. We always stream from worker to server; the server
// can buffer for non-streaming HTTP clients if needed later.
func forceStream(body json.RawMessage) ([]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["stream"] = true
	return json.Marshal(m)
}

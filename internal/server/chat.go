package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/JLAD75/theGriad/internal/proto"
)

// handleChatCompletions accepts an OpenAI-style chat completion request,
// picks an available worker, forwards the request over the worker's WS,
// and streams the SSE response back to the client.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	wc, workerID, err := s.pickWorker()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	reqID := newRequestID()
	ch := s.registerPending(reqID, workerID)
	defer s.closePending(reqID)

	chatReq := proto.ChatRequest{RequestID: reqID, Body: body}
	msg, err := proto.Encode(proto.MsgChatRequest, chatReq)
	if err != nil {
		http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := wc.send(msg); err != nil {
		http.Error(w, "dispatch: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	log.Printf("chat req=%s -> worker=%s (body %d bytes)", reqID, workerID, len(body))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	clientGone := r.Context().Done()
	for {
		select {
		case <-clientGone:
			s.cancelOnWorker(wc, reqID)
			log.Printf("chat req=%s client disconnected", reqID)
			return
		case env := <-ch:
			switch env.Type {
			case proto.MsgChatChunk:
				var chunk proto.ChatChunk
				if err := json.Unmarshal(env.Data, &chunk); err != nil {
					continue
				}
				_, _ = w.Write([]byte(chunk.Data))
				if flusher != nil {
					flusher.Flush()
				}
			case proto.MsgChatDone:
				var done proto.ChatDone
				_ = json.Unmarshal(env.Data, &done)
				if done.Error != "" {
					log.Printf("chat req=%s worker error: %s", reqID, done.Error)
				} else {
					log.Printf("chat req=%s done", reqID)
				}
				return
			}
		}
	}
}

func (s *Server) pickWorker() (*workerConn, string, error) {
	candidates := s.registry.List()
	s.connsMu.RLock()
	defer s.connsMu.RUnlock()
	hasModel := false
	for _, w := range candidates {
		if w.LoadedModel == "" {
			continue
		}
		hasModel = true
		if !w.Idle {
			continue
		}
		if c, ok := s.conns[w.ID]; ok {
			return c, w.ID, nil
		}
	}
	if hasModel {
		return nil, "", errors.New("no idle worker available right now (workers are present but their users are active)")
	}
	return nil, "", errors.New("no worker available with a loaded model")
}

func (s *Server) registerPending(reqID, workerID string) <-chan proto.Envelope {
	ch := make(chan proto.Envelope, 256)
	s.pendingMu.Lock()
	s.pending[reqID] = &pendingReq{ch: ch, workerID: workerID}
	s.pendingMu.Unlock()
	return ch
}

func (s *Server) closePending(reqID string) {
	s.pendingMu.Lock()
	delete(s.pending, reqID)
	s.pendingMu.Unlock()
	// We don't close the channel: with the entry removed from the map,
	// routeChatReply can no longer find it, so no further sends. The
	// channel becomes garbage once the HTTP handler returns.
}

func (s *Server) routeChatReply(env proto.Envelope) {
	var reqID string
	switch env.Type {
	case proto.MsgChatChunk:
		var c proto.ChatChunk
		if err := json.Unmarshal(env.Data, &c); err != nil {
			return
		}
		reqID = c.RequestID
	case proto.MsgChatDone:
		var d proto.ChatDone
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return
		}
		reqID = d.RequestID
	default:
		return
	}
	s.pendingMu.Lock()
	p := s.pending[reqID]
	s.pendingMu.Unlock()
	if p == nil {
		return
	}
	select {
	case p.ch <- env:
	default:
		log.Printf("pending %s buffer full, dropping %s", reqID, env.Type)
	}
}

func (s *Server) cleanupWorkerPending(workerID string) {
	s.pendingMu.Lock()
	var orphaned []chan proto.Envelope
	for id, p := range s.pending {
		if p.workerID == workerID {
			orphaned = append(orphaned, p.ch)
			delete(s.pending, id)
		}
	}
	s.pendingMu.Unlock()
	// Push a synthetic ChatDone with an error so each waiting handler exits
	// the same way it would for a real end-of-stream.
	data, _ := json.Marshal(proto.ChatDone{Error: "worker disconnected"})
	synthetic := proto.Envelope{Type: proto.MsgChatDone, Data: data}
	for _, ch := range orphaned {
		select {
		case ch <- synthetic:
		default:
		}
	}
}

func (s *Server) cancelOnWorker(wc *workerConn, reqID string) {
	msg, err := proto.Encode(proto.MsgCancel, proto.Cancel{RequestID: reqID})
	if err != nil {
		return
	}
	_ = wc.send(msg)
}

func (wc *workerConn) send(msg []byte) error {
	select {
	case wc.out <- msg:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("worker send buffer full")
	}
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

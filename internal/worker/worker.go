package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/JLAD75/theGriad/internal/proto"
	"github.com/JLAD75/theGriad/internal/version"
	"github.com/JLAD75/theGriad/internal/worker/llama"
)

type Config struct {
	ServerURL string
	WorkerID  string

	// Llama spawning. If LlamaBin and ModelPath are both set, the worker
	// supervises a llama-server child process. Otherwise it runs in
	// heartbeat-only mode (useful for connectivity tests).
	LlamaBin   string
	ModelPath  string
	LlamaHost  string
	LlamaPort  int
	ReadyAfter time.Duration // how long to wait for /health
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.ServerURL == "" {
		return errors.New("server URL is required")
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = defaultWorkerID()
	}
	if cfg.ReadyAfter == 0 {
		cfg.ReadyAfter = 2 * time.Minute
	}

	loadedModel := ""
	llamaURL := ""
	var sup *llama.Supervisor
	if cfg.LlamaBin != "" && cfg.ModelPath != "" {
		s, err := llama.New(llama.Config{
			BinPath:   cfg.LlamaBin,
			ModelPath: cfg.ModelPath,
			Host:      cfg.LlamaHost,
			Port:      cfg.LlamaPort,
		})
		if err != nil {
			return err
		}
		if err := s.Start(ctx); err != nil {
			return err
		}
		sup = s
		defer func() { _ = sup.Stop() }()

		if err := sup.WaitReady(ctx, cfg.ReadyAfter); err != nil {
			return err
		}
		loadedModel = filepath.Base(cfg.ModelPath)
		llamaURL = sup.URL()
	} else if cfg.LlamaBin != "" || cfg.ModelPath != "" {
		return errors.New("--llama-server and --model must be set together")
	} else {
		log.Printf("worker running in heartbeat-only mode (no --model)")
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		err := runSession(ctx, cfg, loadedModel, llamaURL, sup)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("worker session ended: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// session is the state of a single active WS connection to the orchestrator.
type session struct {
	conn        *websocket.Conn
	loadedModel string
	llamaURL    string

	out chan []byte // serialized envelopes pending write

	mu       sync.Mutex
	inflight map[string]context.CancelFunc // request_id -> cancel
}

func newSession(conn *websocket.Conn, loadedModel, llamaURL string) *session {
	return &session{
		conn:        conn,
		loadedModel: loadedModel,
		llamaURL:    llamaURL,
		out:         make(chan []byte, 64),
		inflight:    make(map[string]context.CancelFunc),
	}
}

// send queues an envelope for the writeLoop. Drops on full buffer rather
// than blocking the caller (heartbeats and chunks are time-sensitive; a
// stalled WS will surface via the writeLoop's deadline anyway).
func (s *session) send(t proto.MessageType, payload any) {
	msg, err := proto.Encode(t, payload)
	if err != nil {
		log.Printf("encode %s: %v", t, err)
		return
	}
	select {
	case s.out <- msg:
	default:
		log.Printf("send buffer full, dropping %s", t)
	}
}

func (s *session) writeLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-s.out:
			_ = s.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := s.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return err
			}
		}
	}
}

func (s *session) heartbeatLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.send(proto.MsgHeartbeat, proto.Heartbeat{Idle: false}) // TODO: real idle
		}
	}
}

func (s *session) readLoop(ctx context.Context) error {
	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			return err
		}
		env, err := proto.Decode(raw)
		if err != nil {
			log.Printf("decode: %v", err)
			continue
		}
		switch env.Type {
		case proto.MsgChatRequest:
			var req proto.ChatRequest
			if err := json.Unmarshal(env.Data, &req); err != nil {
				log.Printf("decode chat_request: %v", err)
				continue
			}
			go s.handleChat(ctx, req)
		case proto.MsgCancel:
			var c proto.Cancel
			if err := json.Unmarshal(env.Data, &c); err != nil {
				continue
			}
			s.cancelInflight(c.RequestID)
		default:
			// ignore for now
		}
	}
}

func (s *session) registerInflight(reqID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight[reqID] = cancel
}

func (s *session) clearInflight(reqID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, reqID)
}

func (s *session) cancelInflight(reqID string) {
	s.mu.Lock()
	cancel := s.inflight[reqID]
	delete(s.inflight, reqID)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func runSession(ctx context.Context, cfg Config, loadedModel, llamaURL string, sup *llama.Supervisor) error {
	u, err := url.Parse(cfg.ServerURL)
	if err != nil {
		return err
	}
	log.Printf("connecting to %s", u.String())
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	hostname, _ := os.Hostname()
	hello := proto.Hello{
		WorkerID:    cfg.WorkerID,
		Hostname:    hostname,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		NumCPU:      runtime.NumCPU(),
		Version:     version.Version,
		LoadedModel: loadedModel,
	}
	helloMsg, err := proto.Encode(proto.MsgHello, hello)
	if err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.TextMessage, helloMsg); err != nil {
		return err
	}

	_, raw, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	env, err := proto.Decode(raw)
	if err != nil || env.Type != proto.MsgWelcome {
		return errors.New("did not receive welcome from server")
	}
	var welcome proto.Welcome
	_ = json.Unmarshal(env.Data, &welcome)
	hbEvery := welcome.HeartbeatEvery
	if hbEvery <= 0 {
		hbEvery = 5 * time.Second
	}
	log.Printf("connected as %s (server v%s, heartbeat every %s)", welcome.AssignedID, welcome.ServerVersion, hbEvery)

	sess := newSession(conn, loadedModel, llamaURL)
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 3)
	go func() { errCh <- sess.writeLoop(sessCtx) }()
	go func() { errCh <- sess.readLoop(sessCtx) }()
	go func() { sess.heartbeatLoop(sessCtx, hbEvery); errCh <- nil }()

	if sup != nil {
		go func() {
			select {
			case <-sessCtx.Done():
			case <-sup.Done():
				errCh <- errors.New("llama-server exited")
			}
		}()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func defaultWorkerID() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "worker"
	}
	return h + "-" + time.Now().Format("20060102150405")
}

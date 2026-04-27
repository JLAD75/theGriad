package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/JLAD75/theGriad/internal/proto"
	"github.com/JLAD75/theGriad/internal/version"
)

const (
	heartbeatInterval = 5 * time.Second
	heartbeatTimeout  = 20 * time.Second
	pongWait          = 60 * time.Second
)

type Config struct {
	Addr string
}

type Server struct {
	cfg      Config
	registry *Registry
	upgrader websocket.Upgrader

	// live worker connections, keyed by worker id
	connsMu sync.RWMutex
	conns   map[string]*workerConn

	// pending chat requests, keyed by request_id
	pendingMu sync.Mutex
	pending   map[string]*pendingReq
}

// workerConn wraps an active WebSocket connection to a worker. All writes
// go through the out channel so that multiple producers (chat router,
// cancellation, etc.) never write concurrently to the underlying conn.
type workerConn struct {
	id   string
	conn *websocket.Conn
	out  chan []byte
}

type pendingReq struct {
	ch       chan proto.Envelope
	workerID string
}

func New(cfg Config) *Server {
	return &Server{
		cfg:      cfg,
		registry: NewRegistry(),
		conns:    make(map[string]*workerConn),
		pending:  make(map[string]*pendingReq),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/workers", s.handleListWorkers)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/ws/worker", s.handleWorkerWS)

	srv := &http.Server{Addr: s.cfg.Addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	go s.reapLoop(ctx)

	log.Printf("griad server v%s listening on %s", version.Version, s.cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"version": version.Version,
		"workers": s.registry.Count(),
	})
}

func (s *Server) handleListWorkers(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.registry.List())
}

func (s *Server) handleWorkerWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	defer conn.Close()
	s.serveWorker(conn)
}

func (s *Server) serveWorker(conn *websocket.Conn) {
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// 1) expect Hello
	_, raw, err := conn.ReadMessage()
	if err != nil {
		log.Printf("ws read hello: %v", err)
		return
	}
	env, err := proto.Decode(raw)
	if err != nil || env.Type != proto.MsgHello {
		log.Printf("expected hello, got %s (err=%v)", env.Type, err)
		return
	}
	var hello proto.Hello
	if err := json.Unmarshal(env.Data, &hello); err != nil {
		log.Printf("decode hello: %v", err)
		return
	}

	worker := &Worker{
		ID:          hello.WorkerID,
		Hostname:    hello.Hostname,
		OS:          hello.OS,
		Arch:        hello.Arch,
		NumCPU:      hello.NumCPU,
		GPUs:        hello.GPUs,
		Version:     hello.Version,
		LoadedModel: hello.LoadedModel,
	}
	s.registry.Add(worker)
	defer s.registry.Remove(worker.ID)
	log.Printf("worker connected: id=%s host=%s os=%s/%s cpus=%d model=%q", worker.ID, worker.Hostname, worker.OS, worker.Arch, worker.NumCPU, worker.LoadedModel)

	wc := &workerConn{id: worker.ID, conn: conn, out: make(chan []byte, 64)}
	s.connsMu.Lock()
	s.conns[worker.ID] = wc
	s.connsMu.Unlock()
	defer func() {
		s.connsMu.Lock()
		delete(s.conns, worker.ID)
		s.connsMu.Unlock()
		s.cleanupWorkerPending(worker.ID)
	}()

	// 2) send Welcome
	welcome := proto.Welcome{
		AssignedID:     worker.ID,
		ServerVersion:  version.Version,
		HeartbeatEvery: heartbeatInterval,
	}
	msg, _ := proto.Encode(proto.MsgWelcome, welcome)
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		log.Printf("write welcome: %v", err)
		return
	}

	// 3) write loop (reads from wc.out and writes to conn)
	writeErr := make(chan error, 1)
	writeCtx, cancelWrites := context.WithCancel(context.Background())
	defer cancelWrites()
	go func() {
		for {
			select {
			case <-writeCtx.Done():
				writeErr <- nil
				return
			case msg := <-wc.out:
				_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
					writeErr <- err
					return
				}
			}
		}
	}()

	// 4) read loop
	for {
		// Refresh the read deadline on each iteration so any inbound traffic
		// (heartbeat, chat reply) keeps the connection alive. Without this,
		// the deadline set once at session start would expire after pongWait
		// even when the worker is sending heartbeats every 5s.
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			log.Printf("worker %s disconnected: %v", worker.ID, err)
			return
		}
		env, err := proto.Decode(raw)
		if err != nil {
			log.Printf("worker %s decode: %v", worker.ID, err)
			continue
		}
		switch env.Type {
		case proto.MsgHeartbeat:
			var hb proto.Heartbeat
			if err := json.Unmarshal(env.Data, &hb); err != nil {
				continue
			}
			s.registry.Heartbeat(worker.ID, hb.Idle)
		case proto.MsgChatChunk, proto.MsgChatDone:
			s.routeChatReply(env)
		default:
			log.Printf("worker %s unknown msg: %s", worker.ID, env.Type)
		}

		// surface write errors quickly
		select {
		case err := <-writeErr:
			if err != nil {
				log.Printf("worker %s write loop: %v", worker.ID, err)
				return
			}
		default:
		}
	}
}

func (s *Server) reapLoop(ctx context.Context) {
	t := time.NewTicker(heartbeatTimeout / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			for _, w := range s.registry.List() {
				if now.Sub(w.LastHeartbeat) > heartbeatTimeout {
					log.Printf("reaping stale worker %s", w.ID)
					s.registry.Remove(w.ID)
				}
			}
		}
	}
}

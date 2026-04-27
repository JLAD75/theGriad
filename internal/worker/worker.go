package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"os"
	"runtime"
	"time"

	"github.com/gorilla/websocket"

	"github.com/JLAD75/theGriad/internal/proto"
	"github.com/JLAD75/theGriad/internal/version"
)

type Config struct {
	ServerURL string
	WorkerID  string
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.ServerURL == "" {
		return errors.New("server URL is required")
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = defaultWorkerID()
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		err := connectAndServe(ctx, cfg)
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

func connectAndServe(ctx context.Context, cfg Config) error {
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
		WorkerID: cfg.WorkerID,
		Hostname: hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		NumCPU:   runtime.NumCPU(),
		Version:  version.Version,
	}
	msg, err := proto.Encode(proto.MsgHello, hello)
	if err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
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

	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)

	go func() {
		t := time.NewTicker(hbEvery)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				hb := proto.Heartbeat{Idle: false} // TODO: real idle detection
				msg, _ := proto.Encode(proto.MsgHeartbeat, hb)
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()

	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				errCh <- err
				return
			}
		}
	}()

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

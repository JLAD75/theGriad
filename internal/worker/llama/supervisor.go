// Package llama supervises a llama-server child process.
//
// The supervisor takes a path to a llama-server binary and a GGUF model,
// spawns the binary bound to localhost, waits for it to become healthy,
// and stops it cleanly when the parent context is cancelled.
package llama

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type Config struct {
	BinPath   string // path to llama-server binary
	ModelPath string // path to GGUF model
	Host      string // bind host, default 127.0.0.1
	Port      int    // bind port, 0 = auto-pick
	ExtraArgs []string
}

type Supervisor struct {
	cfg Config

	mu     sync.Mutex
	cmd    *exec.Cmd
	port   int
	exited chan struct{}
	exit   error
}

func New(cfg Config) (*Supervisor, error) {
	if cfg.BinPath == "" {
		return nil, errors.New("llama: BinPath is required")
	}
	if cfg.ModelPath == "" {
		return nil, errors.New("llama: ModelPath is required")
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	return &Supervisor{cfg: cfg, exited: make(chan struct{})}, nil
}

// Start launches llama-server. Returns once the process is started (not yet
// healthy). Use WaitReady to block until /health responds OK.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		return errors.New("llama: already started")
	}

	port := s.cfg.Port
	if port == 0 {
		p, err := pickFreePort()
		if err != nil {
			return fmt.Errorf("llama: pick port: %w", err)
		}
		port = p
	}
	s.port = port

	// llama.cpp opens the model with fopen(), which on Windows goes through
	// the ANSI codepage and corrupts non-ASCII characters in the path
	// (accented chars, Cyrillic, CJK, ...). Workaround: spawn with CWD set
	// to the model's directory (Go uses CreateProcessW, so unicode is fine
	// for the CWD itself) and pass only the ASCII basename as -m.
	absModel, err := filepath.Abs(s.cfg.ModelPath)
	if err != nil {
		return fmt.Errorf("llama: resolve model path: %w", err)
	}
	modelDir, modelBase := filepath.Dir(absModel), filepath.Base(absModel)

	// Once we set cmd.Dir, Go's exec on Windows resolves cmd.Path relative
	// to cmd.Dir (not the parent's CWD), so a relative BinPath would be
	// looked up under the model directory. Make BinPath absolute to keep
	// the binary lookup independent of the child CWD.
	absBin, err := filepath.Abs(s.cfg.BinPath)
	if err != nil {
		return fmt.Errorf("llama: resolve bin path: %w", err)
	}

	args := []string{
		"--host", s.cfg.Host,
		"--port", fmt.Sprintf("%d", port),
		"-m", modelBase,
	}
	args = append(args, s.cfg.ExtraArgs...)

	cmd := exec.CommandContext(ctx, absBin, args...)
	cmd.Dir = modelDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("llama: start %s: %w", filepath.Base(s.cfg.BinPath), err)
	}
	s.cmd = cmd

	go forwardLog("llama-server[out]", stdout)
	go forwardLog("llama-server[err]", stderr)

	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.exit = err
		s.mu.Unlock()
		close(s.exited)
	}()

	log.Printf("llama-server started: pid=%d port=%d model=%s", cmd.Process.Pid, port, filepath.Base(s.cfg.ModelPath))
	return nil
}

// WaitReady polls /health until it returns OK or the context is cancelled.
// Loading large models can take tens of seconds.
func (s *Supervisor) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://%s:%d/health", s.cfg.Host, s.port)
	client := &http.Client{Timeout: 2 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.exited:
			return fmt.Errorf("llama: process exited before becoming ready: %v", s.exit)
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				log.Printf("llama-server ready at %s", s.URL())
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("llama: not ready after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Stop terminates the child process. Safe to call multiple times.
func (s *Supervisor) Stop() error {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Kill is harsh but reliable cross-platform. Cleaner shutdown via
	// os.Interrupt is not supported on Windows. Good enough for MVP.
	_ = cmd.Process.Kill()
	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
		return errors.New("llama: child did not exit in time")
	}
	return nil
}

// URL returns the base URL of the OpenAI-compatible API.
func (s *Supervisor) URL() string {
	return fmt.Sprintf("http://%s:%d", s.cfg.Host, s.port)
}

// Done returns a channel closed when the child process exits.
func (s *Supervisor) Done() <-chan struct{} { return s.exited }

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func forwardLog(prefix string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		log.Printf("%s %s", prefix, sc.Text())
	}
}

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/JLAD75/theGriad/internal/server"
	"github.com/JLAD75/theGriad/internal/version"
	"github.com/JLAD75/theGriad/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "server":
		runServer(args)
	case "worker":
		runWorker(args)
	case "chat":
		runChat(args)
	case "version", "-v", "--version":
		fmt.Println("griad", version.Version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `griad — distributed LLM inference grid

usage:
  griad server [--addr :8080]
  griad worker --server ws://host:8080/ws/worker [--id NAME]
               [--llama-server PATH --model PATH [--llama-host H] [--llama-port N]]
  griad chat   --server http://host:8080
  griad version`)
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	_ = fs.Parse(args)

	ctx, stop := signalCtx()
	defer stop()

	s := server.New(server.Config{Addr: *addr})
	if err := s.Run(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func runWorker(args []string) {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	serverURL := fs.String("server", "ws://localhost:8080/ws/worker", "orchestrator WS URL")
	id := fs.String("id", "", "worker id (default: hostname-timestamp)")
	llamaBin := fs.String("llama-server", "", "path to llama-server binary (optional; if unset, runs in heartbeat-only mode)")
	model := fs.String("model", "", "path to GGUF model file (required if --llama-server is set)")
	llamaHost := fs.String("llama-host", "127.0.0.1", "host for the local llama-server bind")
	llamaPort := fs.Int("llama-port", 0, "port for llama-server (0 = pick a free one)")
	_ = fs.Parse(args)

	ctx, stop := signalCtx()
	defer stop()

	if err := worker.Run(ctx, worker.Config{
		ServerURL: *serverURL,
		WorkerID:  *id,
		LlamaBin:  *llamaBin,
		ModelPath: *model,
		LlamaHost: *llamaHost,
		LlamaPort: *llamaPort,
	}); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

func runChat(_ []string) {
	fmt.Fprintln(os.Stderr, "chat TUI: not implemented yet — coming soon")
	os.Exit(1)
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

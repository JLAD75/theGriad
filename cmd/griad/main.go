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
	"github.com/JLAD75/theGriad/internal/tui"
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
	case "model":
		runModel(args)
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
  griad server [--addr :8080] [--models-dir PATH]
  griad worker [--server http://host:8080] [--id NAME]
               [(--model PATH | --catalog-model NAME)
                [--llama-server PATH] [--llama-cache-dir PATH]
                [--cache-dir PATH] [--llama-host H] [--llama-port N]]
  griad chat   [--server http://host:8080]
  griad model  list|pull ...
  griad version`)
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	modelsDir := fs.String("models-dir", ".local/server-models", "directory for the GGUF model catalog")
	_ = fs.Parse(args)

	ctx, stop := signalCtx()
	defer stop()

	s := server.New(server.Config{Addr: *addr, ModelsDir: *modelsDir})
	if err := s.Run(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func runWorker(args []string) {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	serverURL := fs.String("server", "http://localhost:8080", "orchestrator URL (http://host:port; ws:// also accepted)")
	id := fs.String("id", "", "worker id (default: hostname-timestamp)")
	llamaBin := fs.String("llama-server", "", "path to llama-server binary (auto-installed if omitted and a model is set)")
	llamaCacheDir := fs.String("llama-cache-dir", ".local/llama", "where to install llama.cpp when --llama-server is omitted")
	model := fs.String("model", "", "path to a local GGUF file")
	catalogModel := fs.String("catalog-model", "", "name of a model to fetch from the orchestrator catalog (alternative to --model)")
	cacheDir := fs.String("cache-dir", ".local/worker-models", "local cache for models pulled from the catalog")
	llamaHost := fs.String("llama-host", "127.0.0.1", "host for the local llama-server bind")
	llamaPort := fs.Int("llama-port", 0, "port for llama-server (0 = pick a free one)")
	_ = fs.Parse(args)

	ctx, stop := signalCtx()
	defer stop()

	if err := worker.Run(ctx, worker.Config{
		ServerURL:     *serverURL,
		WorkerID:      *id,
		LlamaBin:      *llamaBin,
		LlamaCacheDir: *llamaCacheDir,
		ModelPath:     *model,
		CatalogModel:  *catalogModel,
		CacheDir:      *cacheDir,
		LlamaHost:     *llamaHost,
		LlamaPort:     *llamaPort,
	}); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

func runChat(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	serverURL := fs.String("server", "http://localhost:8080", "orchestrator base URL")
	_ = fs.Parse(args)

	if err := tui.Run(*serverURL); err != nil {
		log.Fatalf("chat: %v", err)
	}
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

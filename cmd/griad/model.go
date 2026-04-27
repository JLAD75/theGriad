package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func runModel(args []string) {
	if len(args) < 1 {
		modelUsage()
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		runModelList(rest)
	case "pull":
		runModelPull(rest)
	case "-h", "--help", "help":
		modelUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown model subcommand: %s\n\n", sub)
		modelUsage()
		os.Exit(2)
	}
}

func modelUsage() {
	fmt.Fprintln(os.Stderr, `griad model — manage the server's GGUF catalog

usage:
  griad model list [--server URL]
  griad model pull NAME URL [--server URL]

examples:
  griad model list
  griad model pull qwen2.5-3b https://registry.ollama.ai/v2/library/qwen2.5/blobs/sha256:5ee4f07cdb9beadbbb293e85803c569b01bd37ed059d2715faa7bb405f31caa6`)
}

func runModelList(args []string) {
	fs := flag.NewFlagSet("model list", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "orchestrator base URL")
	_ = fs.Parse(args)

	resp, err := http.Get(strings.TrimRight(*server, "/") + "/api/models")
	if err != nil {
		fmt.Fprintf(os.Stderr, "request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "server returned %d: %s\n", resp.StatusCode, body)
		os.Exit(1)
	}
	var models []struct {
		Name       string    `json:"name"`
		Filename   string    `json:"filename"`
		Size       int64     `json:"size"`
		ModifiedAt time.Time `json:"modified_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		fmt.Fprintf(os.Stderr, "decode: %v\n", err)
		os.Exit(1)
	}
	if len(models) == 0 {
		fmt.Println("(catalogue vide — utilise `griad model pull NAME URL` pour ajouter un modèle)")
		return
	}
	fmt.Printf("%-40s %12s  %s\n", "NAME", "SIZE", "MODIFIED")
	for _, m := range models {
		fmt.Printf("%-40s %12s  %s\n", m.Name, humanBytes(m.Size), m.ModifiedAt.Local().Format("2006-01-02 15:04"))
	}
}

func runModelPull(args []string) {
	fs := flag.NewFlagSet("model pull", flag.ExitOnError)
	server := fs.String("server", "http://localhost:8080", "orchestrator base URL")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: griad model pull NAME URL [--server URL]")
		os.Exit(2)
	}
	name, url := rest[0], rest[1]

	body, _ := json.Marshal(map[string]string{"name": name, "url": url})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(*server, "/")+"/api/models/pull", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "server returned %d: %s\n", resp.StatusCode, body)
		os.Exit(1)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var lastPrintLen int
	exitCode := 1
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var ev struct {
			Event      string `json:"event"`
			Name       string `json:"name"`
			URL        string `json:"url"`
			Downloaded int64  `json:"downloaded"`
			Total      int64  `json:"total"`
			Message    string `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Event {
		case "start":
			fmt.Printf("téléchargement %s depuis %s\n", ev.Name, ev.URL)
		case "progress":
			line := formatProgress(ev.Downloaded, ev.Total)
			pad := lastPrintLen - len(line)
			if pad < 0 {
				pad = 0
			}
			fmt.Printf("\r%s%s", line, strings.Repeat(" ", pad))
			lastPrintLen = len(line)
		case "done":
			if lastPrintLen > 0 {
				fmt.Println()
			}
			fmt.Printf("✔ %s ajouté au catalogue\n", ev.Name)
			exitCode = 0
		case "error":
			if lastPrintLen > 0 {
				fmt.Println()
			}
			fmt.Fprintf(os.Stderr, "✗ erreur: %s\n", ev.Message)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "stream: %v\n", err)
	}
	os.Exit(exitCode)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatProgress(downloaded, total int64) string {
	if total > 0 {
		pct := float64(downloaded) / float64(total) * 100
		return fmt.Sprintf("  %s / %s  (%.1f%%)", humanBytes(downloaded), humanBytes(total), pct)
	}
	return fmt.Sprintf("  %s téléchargés…", humanBytes(downloaded))
}

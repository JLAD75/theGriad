package worker

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fetchFromCatalog downloads a model named `name` from the orchestrator's
// catalog (GET <httpBase>/api/models/{name}) into cacheDir, returning the
// local path. If a valid cached copy already exists, the network call is
// skipped.
//
// The download streams to a .partial file then atomically renames on
// success, so a partial download from a previous failure never poisons
// the cache. The final file is only used after a magic-bytes GGUF check.
func fetchFromCatalog(ctx context.Context, httpBase, name, cacheDir string) (string, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	target := filepath.Join(cacheDir, name+".gguf")

	if isValidGGUF(target) {
		log.Printf("model %s already cached at %s", name, target)
		return target, nil
	}

	tmp := target + ".partial"
	_ = os.Remove(tmp) // clean any leftover

	endpoint := strings.TrimRight(httpBase, "/") + "/api/models/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("catalog fetch %s: status %d: %s", name, resp.StatusCode, string(buf))
	}

	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer f.Close()

	total := resp.ContentLength
	log.Printf("downloading %s from %s (%s)", name, endpoint, humanSize(total))

	buf := make([]byte, 256*1024)
	var downloaded int64
	lastReport := time.Now()
	for {
		select {
		case <-ctx.Done():
			_ = os.Remove(tmp)
			return "", ctx.Err()
		default:
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return "", werr
			}
			downloaded += int64(n)
			if time.Since(lastReport) > 2*time.Second {
				if total > 0 {
					log.Printf("  …%s / %s (%.1f%%)", humanSize(downloaded), humanSize(total), 100*float64(downloaded)/float64(total))
				} else {
					log.Printf("  …%s", humanSize(downloaded))
				}
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}

	if err := f.Close(); err != nil {
		return "", err
	}
	if !isValidGGUF(tmp) {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("downloaded file is not a valid GGUF")
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", err
	}
	log.Printf("model %s cached: %s (%s)", name, target, humanSize(downloaded))
	return target, nil
}

// isValidGGUF returns true when path exists and starts with "GGUF".
func isValidGGUF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return string(magic[:]) == "GGUF"
}

func humanSize(n int64) string {
	if n <= 0 {
		return "?"
	}
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

// splitWorkerEndpoint normalizes the --server flag for the worker. It
// accepts the HTTP base form (http://host:8080, recommended, consistent
// with `griad chat`) and the legacy WS form (ws://host:8080/ws/worker).
// It returns the HTTP base (used for catalog API calls) and the full WS
// URL (used to dial the orchestrator).
func splitWorkerEndpoint(serverURL string) (httpBase, wsURL string, err error) {
	if serverURL == "" {
		return "", "", fmt.Errorf("--server is required")
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", "", err
	}
	switch u.Scheme {
	case "http", "https":
		base := *u
		base.Path = ""
		base.RawQuery = ""
		httpBase = strings.TrimRight(base.String(), "/")
		ws := *u
		if u.Scheme == "http" {
			ws.Scheme = "ws"
		} else {
			ws.Scheme = "wss"
		}
		ws.Path = strings.TrimRight(u.Path, "/") + "/ws/worker"
		wsURL = ws.String()
	case "ws", "wss":
		wsURL = serverURL
		base := *u
		if u.Scheme == "ws" {
			base.Scheme = "http"
		} else {
			base.Scheme = "https"
		}
		base.Path = ""
		base.RawQuery = ""
		httpBase = strings.TrimRight(base.String(), "/")
	default:
		return "", "", fmt.Errorf("unsupported scheme %q in --server", u.Scheme)
	}
	return httpBase, wsURL, nil
}

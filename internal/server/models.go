package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ModelInfo describes one GGUF model in the server's catalog.
type ModelInfo struct {
	Name       string    `json:"name"`
	Filename   string    `json:"filename"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}

// modelStore manages a directory of GGUF models on the orchestrator.
type modelStore struct {
	dir string
}

func newModelStore(dir string) (*modelStore, error) {
	if dir == "" {
		return nil, errors.New("models dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create models dir: %w", err)
	}
	return &modelStore{dir: dir}, nil
}

func (m *modelStore) list() ([]ModelInfo, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, ModelInfo{
			Name:       strings.TrimSuffix(e.Name(), ".gguf"),
			Filename:   e.Name(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
		})
	}
	return out, nil
}

// path validates the name and returns the on-disk file path.
func (m *modelStore) path(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty name")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", errors.New("invalid name: must not contain path separators or '..'")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
		name += ".gguf"
	}
	full := filepath.Join(m.dir, name)
	// extra paranoia: ensure path stays inside m.dir after Clean
	rel, err := filepath.Rel(m.dir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", errors.New("invalid name: escapes catalog dir")
	}
	return full, nil
}

// pull downloads a GGUF from sourceURL into the catalog under name. The
// download streams to <name>.partial then atomically renames on success.
// progress is invoked periodically with bytes downloaded / total.
func (m *modelStore) pull(ctx context.Context, name, sourceURL string, progress func(downloaded, total int64)) error {
	finalPath, err := m.path(name)
	if err != nil {
		return err
	}
	tmpPath := finalPath + ".partial"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(buf))
	}
	total := resp.ContentLength

	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		// best-effort cleanup of the partial file on error
		if _, err := os.Stat(finalPath); err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	buf := make([]byte, 256*1024)
	var downloaded int64
	lastReport := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			downloaded += int64(n)
			if progress != nil && time.Since(lastReport) > 250*time.Millisecond {
				progress(downloaded, total)
				lastReport = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if progress != nil {
		progress(downloaded, total)
	}

	if err := f.Close(); err != nil {
		return err
	}

	// Verify GGUF magic before committing.
	if err := verifyGGUF(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return err
	}
	return nil
}

func verifyGGUF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if string(magic[:]) != "GGUF" {
		return fmt.Errorf("not a GGUF file (magic=%q)", string(magic[:]))
	}
	return nil
}

// ----- HTTP handlers -----

func (s *Server) handleListModels(w http.ResponseWriter, _ *http.Request) {
	if s.models == nil {
		http.Error(w, "catalog not configured", http.StatusServiceUnavailable)
		return
	}
	list, err := s.models.list()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) handlePullModel(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		http.Error(w, "catalog not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.URL == "" {
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	emit := func(payload any) {
		b, _ := json.Marshal(payload)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	log.Printf("model pull: name=%s url=%s", req.Name, req.URL)
	emit(map[string]any{"event": "start", "name": req.Name, "url": req.URL})

	progress := func(downloaded, total int64) {
		emit(map[string]any{
			"event":      "progress",
			"downloaded": downloaded,
			"total":      total,
		})
	}

	err := s.models.pull(r.Context(), req.Name, req.URL, progress)
	if err != nil {
		log.Printf("model pull %s: %v", req.Name, err)
		emit(map[string]any{"event": "error", "message": err.Error()})
		return
	}
	emit(map[string]any{"event": "done", "name": req.Name})
	log.Printf("model pull done: %s", req.Name)
}

// handleModelByName matches /api/models/{name} and serves the GGUF file
// for download. Supports Range requests via http.ServeFile.
func (s *Server) handleModelByName(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		http.Error(w, "catalog not configured", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/models/")
	if name == "" {
		http.Error(w, "model name required", http.StatusBadRequest)
		return
	}
	path, err := s.models.path(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+info.Name()+`"`)
	http.ServeFile(w, r, path)
}

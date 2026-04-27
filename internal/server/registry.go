package server

import (
	"sync"
	"time"
)

type Worker struct {
	ID            string
	Hostname      string
	OS            string
	Arch          string
	NumCPU        int
	GPUs          []string
	Version       string
	ConnectedAt   time.Time
	LastHeartbeat time.Time
	Idle          bool
}

type Registry struct {
	mu      sync.RWMutex
	workers map[string]*Worker
}

func NewRegistry() *Registry {
	return &Registry{workers: make(map[string]*Worker)}
}

func (r *Registry) Add(w *Worker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w.ConnectedAt = time.Now()
	w.LastHeartbeat = w.ConnectedAt
	r.workers[w.ID] = w
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.workers, id)
}

func (r *Registry) Heartbeat(id string, idle bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[id]
	if !ok {
		return false
	}
	w.LastHeartbeat = time.Now()
	w.Idle = idle
	return true
}

func (r *Registry) List() []Worker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Worker, 0, len(r.workers))
	for _, w := range r.workers {
		out = append(out, *w)
	}
	return out
}

func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.workers)
}

// Package jobs runs long operations (init, drift, sync) in the background and
// lets clients poll them (spec §8: GET /api/v1/jobs/{id}).
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

type Status string

const (
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
)

type Job struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Status   Status    `json:"status"`
	Progress string    `json:"progress,omitempty"`
	Result   any       `json:"result,omitempty"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started_at"`
	Finished time.Time `json:"finished_at,omitempty"`
}

// Handle is what a running job uses to report progress.
type Handle struct {
	m  *Manager
	id string
}

func (h *Handle) Progress(format string, args ...any) {
	h.m.update(h.id, func(j *Job) { j.Progress = fmt.Sprintf(format, args...) })
}

type Manager struct {
	mu   sync.Mutex
	jobs map[string]*Job
	wg   sync.WaitGroup
}

func New() *Manager { return &Manager{jobs: map[string]*Job{}} }

// Start runs fn in a goroutine. ctx is the job's lifetime (use the server's
// context, not the request's) and a panic in fn fails the job, not the server.
func (m *Manager) Start(ctx context.Context, kind string, fn func(ctx context.Context, h *Handle) (any, error)) string {
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	m.mu.Lock()
	m.jobs[id] = &Job{ID: id, Kind: kind, Status: Running, Started: time.Now().UTC()}
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		var res any
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("job panicked: %v", p)
				}
			}()
			res, err = fn(ctx, &Handle{m: m, id: id})
		}()
		m.update(id, func(j *Job) {
			j.Finished = time.Now().UTC()
			if err != nil {
				j.Status, j.Error = Failed, err.Error()
				return
			}
			j.Status, j.Result = Succeeded, res
		})
	}()
	return id
}

func (m *Manager) update(id string, f func(*Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil {
		f(j)
	}
}

// Get returns a copy of the job.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Wait blocks until every started job has finished (used on shutdown and in tests).
func (m *Manager) Wait() { m.wg.Wait() }

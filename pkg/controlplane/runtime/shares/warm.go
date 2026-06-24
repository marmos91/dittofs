package shares

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// Warm job states.
const (
	WarmStateRunning  = "running"
	WarmStateDone     = "done"
	WarmStateFailed   = "failed"
	WarmStateCanceled = "canceled"
)

// maxRetainedJobs bounds the number of terminal (done/failed/canceled) jobs the
// registry keeps so clients can still poll a recently-finished job, without the
// jobs map growing without bound on a long-running server that warms repeatedly.
// Running jobs are never evicted.
const maxRetainedJobs = 32

// WarmJob is the process-local record of an async share-warm operation
// (proactively materializing a share's blocks onto the local tier). It is
// in-memory only: jobs do not survive a restart. All fields are read/written
// under the warmRegistry mutex.
type WarmJob struct {
	ID    string `json:"id"`
	Share string `json:"share"`
	// State is one of WarmState{Running,Done,Failed,Canceled}.
	State       string    `json:"state"`
	BlocksTotal int64     `json:"blocks_total"`
	BlocksDone  int64     `json:"blocks_done"`
	BytesDone   int64     `json:"bytes_done"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Err         string    `json:"error,omitempty"`
}

// clone returns a copy safe to hand outside the registry lock.
func (j *WarmJob) clone() *WarmJob {
	cp := *j
	return &cp
}

// warmRegistry tracks in-flight and completed warm jobs, one active job per
// share. It is process-local and mutex-guarded; the cancel funcs let
// RemoveShare tear down a running warm so it does not outlive its block store.
type warmRegistry struct {
	mu        sync.Mutex
	jobs      map[string]*WarmJob           // jobID -> job
	active    map[string]string             // shareName -> active jobID (running only)
	cancels   map[string]context.CancelFunc // jobID -> detached-context cancel
	counter   int64                         // monotonic, for deterministic job IDs
	completed []string                      // FIFO of terminal jobIDs, bounded by maxRetainedJobs
}

// retire records a job that has reached a terminal state and evicts the oldest
// retained terminal jobs once the window exceeds maxRetainedJobs, so the jobs
// map cannot grow without bound. Caller must hold r.mu.
func (r *warmRegistry) retire(jobID string) {
	r.completed = append(r.completed, jobID)
	for len(r.completed) > maxRetainedJobs {
		oldest := r.completed[0]
		r.completed = r.completed[1:]
		delete(r.jobs, oldest)
	}
}

func newWarmRegistry() *warmRegistry {
	return &warmRegistry{
		jobs:    make(map[string]*WarmJob),
		active:  make(map[string]string),
		cancels: make(map[string]context.CancelFunc),
	}
}

// warmAllResult mirrors engine.WarmResult without coupling the registry to the
// engine package. The adapter in StartWarm bridges the two.
type warmAllResult struct {
	BlocksFetched      int64
	BytesFetched       int64
	BlocksAlreadyLocal int64
}

// start launches a warm run for shareName against run on a DETACHED context
// (derived from context.Background(), not the request ctx) so the job outlives
// the HTTP request that triggered it. If a job is already running for the
// share, the existing job is returned and run is not invoked.
func (r *warmRegistry) start(shareName string, run func(ctx context.Context, progress func(done, total int64)) (warmAllResult, error)) *WarmJob {
	r.mu.Lock()
	if existingID, ok := r.active[shareName]; ok {
		job := r.jobs[existingID].clone()
		r.mu.Unlock()
		return job
	}

	r.counter++
	jobID := fmt.Sprintf("warm-%s-%d", shareName, r.counter)
	job := &WarmJob{
		ID:        jobID,
		Share:     shareName,
		State:     WarmStateRunning,
		StartedAt: time.Now(),
	}
	r.jobs[jobID] = job
	r.active[shareName] = jobID

	ctx, cancel := context.WithCancel(context.Background())
	r.cancels[jobID] = cancel

	snapshot := job.clone()
	r.mu.Unlock()

	go func() {
		progress := func(done, total int64) {
			r.mu.Lock()
			if j, ok := r.jobs[jobID]; ok {
				j.BlocksDone = done
				j.BlocksTotal = total
			}
			r.mu.Unlock()
		}

		res, err := run(ctx, progress)

		// Classify BEFORE clearing the cancel func: cancelling here would set
		// ctx.Err() and misclassify a plain failure as canceled.
		canceled := ctx.Err() != nil

		r.mu.Lock()
		defer r.mu.Unlock()
		j, ok := r.jobs[jobID]
		// Active mapping/cancel are cleared regardless of outcome so a later
		// StartWarm for the same share can launch a fresh job.
		if r.active[shareName] == jobID {
			delete(r.active, shareName)
		}
		if c, ok := r.cancels[jobID]; ok {
			c()
			delete(r.cancels, jobID)
		}
		if !ok {
			return
		}
		j.FinishedAt = time.Now()
		j.BytesDone = res.BytesFetched
		switch {
		case err == nil:
			j.State = WarmStateDone
			j.BlocksDone = j.BlocksTotal
		case canceled:
			j.State = WarmStateCanceled
			j.Err = err.Error()
		default:
			j.State = WarmStateFailed
			j.Err = err.Error()
		}
		// Bound retained terminal jobs so the jobs map cannot grow without limit.
		r.retire(jobID)
		logger.Debug("warm job finished",
			"job", jobID, "share", shareName, "state", j.State,
			"blocks_done", j.BlocksDone, "bytes_done", j.BytesDone, "error", j.Err)
	}()

	return snapshot
}

// get returns a copy of the job by ID.
func (r *warmRegistry) get(jobID string) (*WarmJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[jobID]
	if !ok {
		return nil, false
	}
	return job.clone(), true
}

// cancelForShare cancels any running warm job for shareName. Called from
// RemoveShare so a warm run cannot outlive the block store it materializes
// into.
func (r *warmRegistry) cancelForShare(shareName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	jobID, ok := r.active[shareName]
	if !ok {
		return
	}
	if c, ok := r.cancels[jobID]; ok {
		c()
	}
}

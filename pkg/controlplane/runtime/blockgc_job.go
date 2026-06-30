package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
)

// Block-GC job states.
const (
	GCStateRunning = "running"
	GCStateDone    = "done"
	GCStateFailed  = "failed"
)

// maxRetainedGCJobs bounds the number of terminal GC jobs the registry keeps so
// a client can still poll a recently-finished run, without the jobs map growing
// without bound on a long-running server. A running job is never evicted. GC is
// server-wide and serializes on the engine's per-root lock, so only a handful of
// jobs are ever in flight; a small window suffices.
const maxRetainedGCJobs = 16

// GCJob is the process-local record of an async block-store GC run. It is
// in-memory only (jobs do not survive a restart) and every field is read/written
// under the gcRegistry mutex. The async run executes on a context detached from
// the triggering HTTP request, so a request/client timeout cannot abort the
// mark phase mid-run (the synchronous endpoint's failure mode on a large or
// snapshot-heavy deployment) (#1433).
type GCJob struct {
	ID        string `json:"id"`
	State     string `json:"state"` // GCState{Running,Done,Failed}
	Share     string `json:"share"`
	Reconcile bool   `json:"reconcile"`
	DryRun    bool   `json:"dry_run"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// Live progress, updated from the engine mark/sweep callbacks while the run
	// is in flight (best-effort liveness). The authoritative final counts live
	// in Stats once State is terminal.
	HashesMarked   int64 `json:"hashes_marked"`
	ObjectsScanned int64 `json:"objects_scanned"`
	ObjectsSwept   int64 `json:"objects_swept"`
	BytesFreed     int64 `json:"bytes_freed"`

	// Stats is the final accumulated GCStats, set when the run finishes.
	Stats *engine.GCStats `json:"stats,omitempty"`
	Err   string          `json:"error,omitempty"`
}

// clone returns a copy safe to hand outside the registry lock.
func (j *GCJob) clone() *GCJob {
	cp := *j
	if j.Stats != nil {
		s := *j.Stats
		cp.Stats = &s
	}
	return &cp
}

// gcRegistry tracks the single in-flight GC run plus a bounded window of
// recently-finished runs for polling. GC is server-wide and the engine
// serializes concurrent runs on a per-root lock, so the registry permits at most
// one active job: a start request while a run is in flight returns the running
// job rather than launching a second.
type gcRegistry struct {
	mu        sync.Mutex
	jobs      map[string]*GCJob
	activeID  string                        // "" when no run is in flight
	cancels   map[string]context.CancelFunc // jobID -> detached-context cancel
	counter   int64
	completed []string // FIFO of terminal jobIDs, bounded by maxRetainedGCJobs
}

func newGCRegistry() *gcRegistry {
	return &gcRegistry{
		jobs:    make(map[string]*GCJob),
		cancels: make(map[string]context.CancelFunc),
	}
}

// retire records a terminal job and evicts the oldest retained terminal jobs
// once the window exceeds maxRetainedGCJobs. Caller must hold r.mu.
func (r *gcRegistry) retire(jobID string) {
	r.completed = append(r.completed, jobID)
	for len(r.completed) > maxRetainedGCJobs {
		oldest := r.completed[0]
		r.completed = r.completed[1:]
		delete(r.jobs, oldest)
	}
}

// start launches a GC run on a DETACHED context (derived from
// context.Background(), not the request ctx) so the job outlives the HTTP
// request that triggered it. If a run is already in flight, the existing job is
// returned and run is not invoked. run receives a progress sink it must forward
// to the engine. Returns a snapshot of the (new or already-running) job.
func (r *gcRegistry) start(share string, dryRun, reconcile bool, run func(ctx context.Context, progress func(engine.GCStats)) (*engine.GCStats, error)) *GCJob {
	r.mu.Lock()
	if r.activeID != "" {
		job := r.jobs[r.activeID].clone()
		r.mu.Unlock()
		return job
	}

	r.counter++
	// Opaque, slash-free id so the {job_id} poll route is unaffected by the
	// (slash-bearing) share name.
	jobID := fmt.Sprintf("gc-%d", r.counter)
	job := &GCJob{
		ID:        jobID,
		State:     GCStateRunning,
		Share:     share,
		Reconcile: reconcile,
		DryRun:    dryRun,
		StartedAt: time.Now(),
	}
	r.jobs[jobID] = job
	r.activeID = jobID

	ctx, cancel := context.WithCancel(context.Background())
	r.cancels[jobID] = cancel

	snapshot := job.clone()
	r.mu.Unlock()

	go func() {
		progress := func(s engine.GCStats) {
			r.mu.Lock()
			if j, ok := r.jobs[jobID]; ok {
				// Merge by max: the mark callback reports only HashesMarked, the
				// sweep callback reports per-invocation totals; max keeps the
				// display monotonic across the run's phases/invocations.
				j.HashesMarked = max(j.HashesMarked, s.HashesMarked)
				j.ObjectsScanned = max(j.ObjectsScanned, s.ObjectsScanned)
				j.ObjectsSwept = max(j.ObjectsSwept, s.ObjectsSwept)
				j.BytesFreed = max(j.BytesFreed, s.BytesFreed)
			}
			r.mu.Unlock()
		}

		stats, err := run(ctx, progress)

		r.mu.Lock()
		defer r.mu.Unlock()
		if r.activeID == jobID {
			r.activeID = ""
		}
		if c, ok := r.cancels[jobID]; ok {
			c()
			delete(r.cancels, jobID)
		}
		j, ok := r.jobs[jobID]
		if !ok {
			return
		}
		j.FinishedAt = time.Now()
		if err != nil {
			j.State = GCStateFailed
			j.Err = err.Error()
		} else {
			j.State = GCStateDone
			j.Stats = stats
			if stats != nil {
				j.HashesMarked = stats.HashesMarked
				j.ObjectsScanned = stats.ObjectsScanned
				j.ObjectsSwept = stats.ObjectsSwept
				j.BytesFreed = stats.BytesFreed
			}
		}
		r.retire(jobID)
		logger.Info("block GC job finished",
			"job", jobID, "share", share, "reconcile", reconcile,
			"state", j.State, "objects_swept", j.ObjectsSwept,
			"bytes_freed", j.BytesFreed, "error", j.Err)
	}()

	return snapshot
}

// get returns a copy of the job by ID.
func (r *gcRegistry) get(jobID string) (*GCJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[jobID]
	if !ok {
		return nil, false
	}
	return job.clone(), true
}

// cancelActive cancels any in-flight GC run. Called on server shutdown so a
// long mark/sweep does not outlive the process's stores.
func (r *gcRegistry) cancelActive() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeID == "" {
		return
	}
	if c, ok := r.cancels[r.activeID]; ok {
		c()
	}
}

// StartBlockGC launches (or returns the already-running) async block-store GC
// job. When reconcile is true the run reaps stranded file_blocks rows across all
// shares before sweeping both tiers; otherwise it runs the share-scoped sweep.
// The run executes on a detached context so a request/client timeout cannot
// abort it. Returns a snapshot of the job; poll GetGCJobStatus(job.ID) for
// completion.
//
// For the share-scoped (non-reconcile) path the share is validated up front so
// an unknown share fails fast with an ErrShareNotFound-wrapped error rather than
// surfacing only later as a failed job. Reconcile is server-wide and skips the
// per-share check.
func (r *Runtime) StartBlockGC(shareName string, dryRun, reconcile bool) (*GCJob, error) {
	if !reconcile {
		if _, err := r.sharesSvc.GetGCStateDirForShare(shareName); err != nil {
			return nil, err
		}
	}
	return r.gcReg.start(shareName, dryRun, reconcile, func(ctx context.Context, progress func(engine.GCStats)) (*engine.GCStats, error) {
		if reconcile {
			return r.runBlockGCReconcile(ctx, dryRun, progress)
		}
		return r.runBlockGCForShare(ctx, shareName, dryRun, progress)
	}), nil
}

// GetGCJobStatus returns a snapshot of a GC job by ID, or false if unknown
// (never started, or evicted from the retained-terminal window).
func (r *Runtime) GetGCJobStatus(jobID string) (*GCJob, bool) {
	return r.gcReg.get(jobID)
}

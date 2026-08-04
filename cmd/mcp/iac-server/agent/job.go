// Job-based async execution for long-running LLM tools.
//
// MCP has no native async tool calls, and tool execution timeouts are a
// client-side concern that varies per client — our agents routinely run
// 2-3 minutes (specify_resources querying flavors, estimate_cost pricing),
// which no client can be assumed to tolerate synchronously. The job
// pattern solves this without assuming anything about the client: the
// tool call returns immediately with a job_id, the client polls
// get_job_result (with optional long-poll wait), and the server runs the
// actual work in a background goroutine.
//
// Jobs persist to <workDir>/jobs/job-<id>.json so a server restart does
// not lose them. Same-deployment jobs run serially (a deployment's dag.json
// is a shared mutable contract); different deployments run in parallel.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	openagent "github.com/yusheng-g/openagent-go"
)

// JobStatus is the lifecycle state of an async job.
type JobStatus string

const (
	JobRunning  JobStatus = "running"
	JobDone     JobStatus = "done"
	JobFailed   JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// maxConcurrentJobs bounds parallel job execution across all deployments —
// each job runs a full LLM agent, so unbounded parallelism would flood the
// model API.
const maxConcurrentJobs = 4

// JobTimeout bounds a single job's execution.
const JobTimeout = 15 * time.Minute

// outputLimits bound the model output log: each entry is truncated and
// the log keeps only the most recent entries, so a long agent run cannot
// balloon the job file.
const (
	outputEntryMax = 2000
	outputMaxCount = 100
)

// Job is the persisted state of an async tool execution.
type Job struct {
	ID           string          `json:"id"`
	Tool         string          `json:"tool"`
	Status       JobStatus       `json:"status"`
	DeploymentID string          `json:"deployment_id,omitempty"`
	ProgressMsg  string          `json:"progress_msg,omitempty"`
	ProgressCur  float64         `json:"progress_cur,omitempty"`
	ProgressTot  float64         `json:"progress_tot,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        string          `json:"error,omitempty"`
	Outputs      []string        `json:"outputs,omitempty"` // server LLM's per-turn text
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

// runningJob tracks a job that has been submitted, with its cancel handle
// so a newer submission for the same deployment can supersede it.
type runningJob struct {
	job    *Job
	cancel context.CancelFunc
}

// JobManager owns job lifecycle: submission, progress, persistence, and
// per-deployment supersession.
type JobManager struct {
	mu      sync.Mutex // guards running, and serializes job file writes
	jobsDir string
	running map[string]*runningJob // deployment id -> latest submitted job
	sem     chan struct{}          // global concurrency limit
}

// NewJobManager creates a manager rooted at jobsDir (created if missing).
// Stale job files older than a day are cleaned up at startup.
func NewJobManager(jobsDir string) *JobManager {
	_ = os.MkdirAll(jobsDir, 0o755)
	m := &JobManager{
		jobsDir: jobsDir,
		running: make(map[string]*runningJob),
		sem:     make(chan struct{}, maxConcurrentJobs),
	}
	m.cleanupStale()
	return m
}

// cleanupStale removes job files older than 24h — a client may poll a job
// for a while after it finishes, but past that the file is dead weight.
func (m *JobManager) cleanupStale() {
	entries, err := os.ReadDir(m.jobsDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "job-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(m.jobsDir, e.Name()))
		}
	}
}

// Submit registers a job and runs fn in a background goroutine.
// It returns the job id immediately. fn's context carries the progress
// callback (writing to the job file) and a 15-minute deadline; panics are
// recovered into a failed job.
//
// When deploymentID is non-empty, jobs for the same deployment serialize:
// fn starts only after any prior job for that deployment finishes, so the
// shared dag.json/cost.json are never written concurrently.
func (m *JobManager) Submit(ctx context.Context, deploymentID, tool string, fn func(ctx context.Context) (string, error)) (string, error) {
	job := &Job{
		ID:           fmt.Sprintf("job-%d", time.Now().UnixNano()),
		Tool:         tool,
		Status:       JobRunning,
		DeploymentID: deploymentID,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.save(job); err != nil {
		return "", err
	}

	// The submit request's ctx is cancelled when the tool call returns
	// (which is immediate for async jobs) — the job runs on its own
	// context, bounded only by JobTimeout, and can be superseded by a
	// newer submission for the same deployment.
	runCtx, cancel := context.WithTimeout(context.Background(), JobTimeout)

	// Supersede any prior job for the same deployment: cancel it so the
	// client's retry/submit always acts on the latest request instead of
	// queueing behind a stale run. The superseded job reports cancelled.
	m.mu.Lock()
	if prev, ok := m.running[deploymentID]; ok {
		prev.cancel()
		m.cancelLocked(prev.job)
	}
	m.running[deploymentID] = &runningJob{job: job, cancel: cancel}
	m.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.fail(job, fmt.Sprintf("job panicked: %v", r))
			}
		}()
		// Global concurrency limit — wait for a slot (or supersession).
		select {
		case m.sem <- struct{}{}:
		case <-runCtx.Done():
			m.fail(job, "cancelled before start")
			return
		}
		defer func() { <-m.sem }()

		// Progress + model outputs write to the job file so get_job_result
		// reports live intermediate output.
		runCtx = openagent.WithProgress(runCtx, func(msg string, cur, tot float64) {
			m.progress(job, msg, cur, tot)
		})
		runCtx = WithJobOutputs(runCtx, func(content string) {
			m.appendOutput(job, content)
		})

		result, err := fn(runCtx)
		if err != nil {
			m.fail(job, err.Error())
			return
		}
		m.done(job, result)
	}()
	return job.ID, nil
}

// Get returns the job's current state, or nil if it does not exist.
// When wait > 0, it long-polls: blocks until the job finishes or the wait
// elapses, then returns the latest state (clients poll less often).
func (m *JobManager) Get(ctx context.Context, id string, wait time.Duration) (*Job, error) {
	deadline := time.Now().Add(wait)
	for {
		job, err := m.load(id)
		if err != nil {
			return nil, err
		}
		if job == nil {
			return nil, nil
		}
		if job.Status != JobRunning || wait <= 0 || time.Now().After(deadline) {
			return job, nil
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return job, nil
		}
	}
}

// cancelLocked marks a superseded job cancelled and persists it. Caller
// holds m.mu; the superseded goroutine may still be writing the same file,
// so the actual write happens under the same lock via saveLocked.
func (m *JobManager) cancelLocked(job *Job) {
	job.Status = JobCancelled
	job.Error = "superseded by a newer submission for this deployment"
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	_ = m.saveLocked(job)
}

func (m *JobManager) save(job *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked(job)
}

// saveLocked writes the job file; caller holds m.mu.
func (m *JobManager) saveLocked(job *Job) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	if err := os.WriteFile(filepath.Join(m.jobsDir, job.ID+".json"), data, 0644); err != nil {
		return fmt.Errorf("write job: %w", err)
	}
	return nil
}

func (m *JobManager) load(id string) (*Job, error) {
	if id == "" || filepath.Base(id) != id {
		return nil, fmt.Errorf("invalid job id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(m.jobsDir, id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read job: %w", err)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("parse job: %w", err)
	}
	return &job, nil
}

// jobOutputKey is the ctx value carrying a job's output sink.
type jobOutputKey struct{}

// WithJobOutputs returns a ctx whose model outputs are appended to the
// given job's log. The runtime's observer reads this via
// JobOutputsFromContext; absence is fine (synchronous calls).
func WithJobOutputs(ctx context.Context, sink func(string)) context.Context {
	return context.WithValue(ctx, jobOutputKey{}, sink)
}

// JobOutputsFromContext returns the job output sink from ctx, or nil.
func JobOutputsFromContext(ctx context.Context) func(string) {
	f, _ := ctx.Value(jobOutputKey{}).(func(string))
	return f
}

// appendOutput adds the server LLM's per-turn text to the job's output
// log (truncated, bounded).
func (m *JobManager) appendOutput(job *Job, content string) {
	m.mu.Lock()
	job.Outputs = append(job.Outputs, truncate(content, outputEntryMax))
	if len(job.Outputs) > outputMaxCount {
		job.Outputs = job.Outputs[len(job.Outputs)-outputMaxCount:]
	}
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.mu.Unlock()
	_ = m.save(job)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut at a rune boundary.
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}

func (m *JobManager) progress(job *Job, msg string, cur, tot float64) {
	m.mu.Lock()
	job.ProgressMsg = msg
	job.ProgressCur = cur
	job.ProgressTot = tot
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.mu.Unlock()
	_ = m.save(job)
}

func (m *JobManager) done(job *Job, result string) {
	m.mu.Lock()
	job.Status = JobDone
	job.Result = json.RawMessage(result)
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.mu.Unlock()
	_ = m.save(job)
}

func (m *JobManager) fail(job *Job, errMsg string) {
	m.mu.Lock()
	if job.Status == JobCancelled {
		// Superseded jobs keep their "cancelled" status — the fn's
		// context-cancelled error must not overwrite the supersession
		// marker.
		m.mu.Unlock()
		return
	}
	job.Status = JobFailed
	job.Error = errMsg
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.mu.Unlock()
	_ = m.save(job)
}

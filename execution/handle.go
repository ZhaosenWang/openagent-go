package execution

import (
	"context"
	"sync"

	openagent "github.com/yusheng-g/openagent-go"
)

// ExecutionHandle tracks one running tool call (a job). The kernel starts
// all approved calls, then waits in call order and collects outputs —
// ordering is preserved while execution runs concurrently.
type ExecutionHandle interface {
	// ID returns the tool call ID.
	ID() string
	// Output returns the final RoleTool message (valid after Wait returns
	// nil).
	Output() openagent.Message
	// Wait blocks until the job finishes (success, error, or cancel).
	Wait(ctx context.Context) error
	// Cancel aborts the job. The kernel calls it for all in-flight jobs
	// when the run context is cancelled.
	Cancel()
}

// job is the concrete handle implementation.
type job struct {
	call    openagent.ToolCall
	session openagent.Session
	ch      chan<- openagent.StreamEvent

	done   chan struct{}
	output openagent.Message
	err    error

	cancelOnce sync.Once
	cancelFn   context.CancelFunc

	once sync.Once
}

// startJob launches a tool call as a background job.
func (e *ExecutionRuntime) startJob(ctx context.Context, session openagent.Session, call openagent.ToolCall, ch chan<- openagent.StreamEvent) *job {
	jobCtx, cancel := context.WithCancel(ctx)
	j := &job{
		call:     call,
		session:  session,
		ch:       ch,
		done:     make(chan struct{}),
		cancelFn: cancel,
	}
	go func() {
		defer close(j.done)
		defer func() {
			if rec := recover(); rec != nil {
				j.err = &jobPanic{rec: rec}
				j.output = openagent.Message{
					Role:       openagent.RoleTool,
					ToolCallID: call.ID,
					Content:    "tool panic: " + panicString(rec),
				}
			}
		}()
		// Retry on retryable tool errors happens inside execute.
		j.output = e.execute(jobCtx, session, call, ch)
		j.err = j.output.Result.AsError()
	}()
	return j
}

// isRetryable reports whether a tool result carries a Retryable error.
func isRetryable(msg openagent.Message) bool {
	return msg.Result != nil && msg.Result.Error != nil && msg.Result.Error.Retryable
}

func (j *job) ID() string { return j.call.ID }

func (j *job) Output() openagent.Message { return j.output }

func (j *job) Wait(ctx context.Context) error {
	select {
	case <-j.done:
		return j.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (j *job) Cancel() {
	j.cancelOnce.Do(j.cancelFn)
}

// jobPanic wraps a recovered panic value as an error.
type jobPanic struct{ rec any }

func (p *jobPanic) Error() string { return panicString(p.rec) }

// panicString formats a recovered panic value.
func panicString(rec any) string {
	if err, ok := rec.(error); ok {
		return err.Error()
	}
	if s, ok := rec.(string); ok {
		return s
	}
	return "unknown panic"
}

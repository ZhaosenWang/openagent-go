package agent

import (
	"context"

	openagent "github.com/yusheng-g/openagent-go"
)

// jobObserver streams the server LLM's per-turn text output into job logs
// via the runtime's stage observer. The model.call stage carries the full
// assistant content in its Detail; this observer forwards it to the active
// job's output log when the context carries one.
//
// It is a pure observer (read-only, per the RunObserver contract) — unlike
// hooks which may mutate results. The same instance is shared by all
// agents; the job output sink is read from each run's context, so
// concurrent jobs are isolated and non-job runs are a no-op.
type jobObserver struct{}

// ObserveStage forwards model output content to the job output log.
func (o *jobObserver) ObserveStage(ctx context.Context, ev openagent.StageEvent) {
	if ev.Name != openagent.StageModelCall || ev.Phase != "leave" {
		return
	}
	sink := JobOutputsFromContext(ctx)
	if sink == nil {
		return
	}
	if content, ok := ev.Detail["content"].(string); ok && content != "" {
		sink(content)
	}
}

var _ openagent.RunObserver = (*jobObserver)(nil)

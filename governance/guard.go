package governance

import (
	"context"

	openagent "github.com/yusheng-g/openagent-go"
)

// GuardInput is passed to InputGuard.Check().
type GuardInput struct {
	Session openagent.Session
	Input   openagent.Message
	History []openagent.Message // recent conversation for context
}

// GuardOutput is passed to OutputGuard.Check().
type GuardOutput struct {
	Session openagent.Session
	Output  openagent.Message // the model's response
	History []openagent.Message
}

// GuardResult is the result of a guard check.
type GuardResult struct {
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason,omitempty"`
	Tripwire bool   `json:"tripwire"` // true = terminate the entire run
}

// InputGuard checks input before it reaches the model.
// nil InputGuard = all input is allowed.
type InputGuard interface {
	Check(ctx context.Context, input GuardInput) GuardResult
}

// OutputGuard checks model output before it is returned or tools are executed.
// nil OutputGuard = all output is allowed.
type OutputGuard interface {
	Check(ctx context.Context, output GuardOutput) GuardResult
}

package main

import (
	"context"

	openagent "github.com/yusheng-g/openagent-go"
	"github.com/yusheng-g/openagent-go/governance"
)

// approveRequest bridges the synchronous Approver interface to the
// asynchronous bubbletea main loop via channels.
type approveRequest struct {
	call    openagent.ToolCall
	respond chan approveResponse
}

type approveResponse struct {
	allowed bool
	reason  string
}

// TUIApprover implements governance.HumanApprover. When Ask is called by
// the policy engine, it sends a request to the bubbletea main loop and
// blocks until the user makes a decision (Y/N keypress).
type TUIApprover struct {
	requests chan<- approveRequest
}

func (a *TUIApprover) Ask(ctx context.Context, call openagent.ToolCall, _ openagent.FunctionDefinition, _ openagent.Session) (governance.Decision, error) {
	resp := make(chan approveResponse, 1)
	select {
	case a.requests <- approveRequest{call: call, respond: resp}:
	case <-ctx.Done():
		return governance.Decision{Action: governance.Deny, Reason: ctx.Err().Error()}, nil
	}

	select {
	case <-ctx.Done():
		return governance.Decision{Action: governance.Deny, Reason: ctx.Err().Error()}, nil
	case r := <-resp:
		if r.allowed {
			return governance.Decision{Action: governance.Allow, Reason: r.reason}, nil
		}
		return governance.Decision{Action: governance.Deny, Reason: r.reason}, nil
	}
}

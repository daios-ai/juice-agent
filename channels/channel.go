// Package channels is the agent's intake/egress framework. The root package holds
// what every channel shares — the Channel interface, the kernel seam a channel
// speaks through, the generic Service driver (service.go), and the reply loop
// guard (loopguard.go) — and imports only `agent`, staying kernel-free like the
// loop. Each concrete channel is its own subpackage (channels/task, channels/http,
// channels/chat, …) that imports this package plus `agent`; the juice client is
// reached only through the interfaces here, implemented once by a cmd adapter. To
// add a channel, add a subpackage — never new kernel code outside cmd.
package channels

import (
	"context"

	"github.com/daios-ai/juice-agent/agent"
)

// Channel is one job-intake surface. Start begins producing jobs (submitting each
// via the callback) and returns once producers are running; Deliver reports a
// finished job's outcome; Steering supplies optional mid-job input; Stop halts
// production. Every channel — the generic task channel, HTTP, and dedicated
// service channels like chat — satisfies this one interface.
type Channel interface {
	Name() string
	Start(ctx context.Context, submit func(agent.Job)) error
	Deliver(jobID string, out agent.Outcome)
	Steering(jobID string) []agent.Message
	Stop()
}

// TaskInfo is a waiting kernel task (D6) addressed to the agent, projected to
// what a channel needs. The cmd adapter maps juice.Task into this, keeping
// channels independent of the kernel client.
//
// The task is the message: Owner is the sender (the channel and session
// identity), PartialArgs is the envelope, ActionRef is the onward action the reply
// is delivered through (its completion), and AllowedInput is the reply contract
// (the onward action's input_schema minus the pre-bound keys). Parties are
// addresses (handle@kernel).
type TaskInfo struct {
	ID             string
	Owner          string         // sender/payer — the channel and session key
	RequiredCaller string         // whom the task is addressed to; ours to do iff it is us
	ActionRef      string         // the onward (completion-target) action
	PartialArgs    map[string]any // the request envelope
	AllowedInput   map[string]any // reply contract: input_schema \ keys(partial_args)
	CreatedBy      string
}

// TaskSource is the narrow kernel surface the task channel needs: list the
// agent's waiting tasks, complete one, and classify a failure as worth retrying.
// Implemented by the cmd adapter; faked in tests.
type TaskSource interface {
	WaitingTasks(ctx context.Context) ([]TaskInfo, error)
	CompleteTask(ctx context.Context, id string, args map[string]any) error
	// Transient reports whether an error is worth retrying (vs. permanent).
	Transient(err error) bool
}

// Kernel is the full kernel surface a notified service channel (e.g. chat) needs:
// the task operations (via TaskSource) plus running actions, and the grant
// operations for on-demand provisioning. It is expressed in channel terms (no juice
// types), so channel subpackages stay kernel-free; the cmd adapter implements it
// once over the juice client.
//
// This is distinct from cmd's own `Kernel` (the model/executor surface —
// Decide/Lookup/Run/Rate): that is what the agent loop reasons with; this is the
// task-and-grant surface a channel delivers through.
type Kernel interface {
	TaskSource
	// Run dispatches an action and returns its result map.
	Run(ctx context.Context, action string, args map[string]any) (map[string]any, error)
	// Connectors lists the grant directories the agent already holds, for
	// provisioning idempotency.
	Connectors(ctx context.Context) ([]string, error)
	// ConnectGrant attaches a static token as the grant for selector.
	ConnectGrant(ctx context.Context, selector, token string) error
	// GrantRequired reports whether an error is a missing-grant rejection, so the
	// channel can provision the grant and retry.
	GrantRequired(err error) bool
}

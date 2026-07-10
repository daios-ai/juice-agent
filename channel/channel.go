// Package channel is the agent's intake abstraction (pi's "surfaces" seam):
// where tasks come from and where outcomes go. Channels are deliberately not
// bound to kernel-native transports — an http channel is Juice-independent, a
// step channel is kernel-native — so the loop never knows a task's origin.
package channel

import (
	"context"

	"github.com/daios-ai/juice-agent/agent"
)

// Channel is one task-intake surface. Start begins producing tasks (submitting
// each via the callback) and returns once producers are running; Deliver reports
// a finished task's outcome; Steering supplies optional mid-task input; Stop
// halts production.
type Channel interface {
	Name() string
	Start(ctx context.Context, submit func(agent.Task)) error
	Deliver(taskID string, out agent.Outcome)
	Steering(taskID string) []agent.Message
	Stop()
}

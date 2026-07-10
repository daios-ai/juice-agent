// Package agent is the pi-style core loop of the Juice agent: a hand-owned
// LLM<->tools<->history cycle whose tool set is the Juice action catalog.
//
// It is deliberately pure. It imports neither net/http nor the juice client;
// it depends only on the small interfaces declared here (Decider, Catalog,
// Runner, Transcript), which the policy and client layers implement. The loop
// therefore runs entirely against fakes in tests, with no kernel and no LLM.
package agent

import "context"

// Role labels a transcript message. The values map onto @sys/llm/decide's
// message roles (system, user, assistant, tool).
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one record in a task transcript. An assistant selection carries
// Action+Args; a tool result carries Action+Result (+TxID, +IsError). Plain
// system/user messages carry Content. Kind tags synthetic records (e.g.
// "budget", "summary", "interrupted") so persistence and recovery can find them.
type Message struct {
	Role    Role           `json:"role"`
	Content string         `json:"content,omitempty"`
	Action  string         `json:"action,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
	TxID    string         `json:"tx_id,omitempty"`
	IsError bool           `json:"is_error,omitempty"`
	Kind    string         `json:"kind,omitempty"`
}

// ActionStats is an action's kernel track record (GET /v1/stats/{id}). Known is
// false when no stats row was fetched (an untested action), so callers can
// distinguish "zero uses" from "unknown".
type ActionStats struct {
	Uses            int     `json:"uses"`
	Successes       int     `json:"successes"`
	Failures        int     `json:"failures"`
	LatencyEstimate float64 `json:"latency_estimate"`
	RatingEstimate  float64 `json:"rating_estimate"`
	RatingCount     int     `json:"rating_count"`
	Known           bool    `json:"-"`
}

// Candidate is a callable action in the working set: its @owner/name ref, the
// contract fields lookup returns, its price (the subtree bound), and its stats.
type Candidate struct {
	Ref         string         `json:"ref"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Price       int            `json:"price"`
	Stats       ActionStats    `json:"stats"`
}

// Selection is one @sys/llm/decide result: the chosen action, args already
// validated against that action's input_schema by the kernel, and an optional
// natural-language message.
type Selection struct {
	Action  string         `json:"action"`
	Args    map[string]any `json:"args,omitempty"`
	Message string         `json:"message,omitempty"`
}

// RunResult is the outcome of dispatching a tool call through Runner.
type RunResult struct {
	Result map[string]any
	TxID   string
}

// Decider performs one model round-trip: given the conversation and the set of
// selectable action refs, it returns the next tool selection. Implemented by the
// juice client over @sys/llm/decide.
type Decider interface {
	Decide(ctx context.Context, messages []Message, actions []string) (*Selection, error)
}

// Catalog discovers actions and reads their metadata. Lookup ranks active
// actions for a query (@sys/lookup); Meta fetches an action's price and stats.
type Catalog interface {
	Lookup(ctx context.Context, query string, limit int) ([]Candidate, error)
	Meta(ctx context.Context, ref string) (price int, stats ActionStats, err error)
}

// Runner dispatches a tool call: POST /v1/run on the action. A kernel-level
// failure (the action ran and failed, or was rejected) is returned as an error.
type Runner interface {
	Run(ctx context.Context, action string, args map[string]any) (RunResult, error)
}

// Transcript is the durable, possibly-compacted working memory. Append persists
// one record; Messages returns the current message list to send to the model
// (which the implementation may have compacted). The core keeps no other state.
type Transcript interface {
	Append(ctx context.Context, m Message) error
	Messages() []Message
}

// Intrinsic is an agent-native capability the harness intercepts instead of
// dispatching through Runner. Its action is nonetheless registered in the kernel
// (zero price) so decide can select it and validate its args. Handle returns the
// tool result, whether the run should terminate, and an error.
type Intrinsic struct {
	Handle func(ctx context.Context, args map[string]any) (result map[string]any, terminal bool, err error)
}

// Task is one unit of work: a transcript seed, an originating channel, and a
// credit budget bounding total spend.
type Task struct {
	ID       string
	Channel  string
	Messages []Message
	Budget   int
}

// Status is a task's terminal disposition.
type Status string

const (
	StatusAnswered    Status = "answered"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
)

// Outcome is what a completed loop reports back through its channel.
type Outcome struct {
	Status Status         `json:"status"`
	Answer map[string]any `json:"answer,omitempty"`
	Reason string         `json:"reason,omitempty"`
	Spent  int            `json:"spent"`
	Turns  int            `json:"turns"`
}

// Config is the loop's static policy: the reply/lookup action refs, the caps,
// the working-set bound, and the intrinsics table.
type Config struct {
	ReplyAction    string
	LookupAction   string
	AlwaysRefs     []string // refs always offered to decide (e.g. @sys/lookup, @sys/llm/*)
	MaxTurns       int
	CandidateLimit int
	LookupLimit    int
	// MaxDecideErrors bounds consecutive invalid model selections before the run
	// fails; each is fed back for self-correction. Default 3 when zero.
	MaxDecideErrors int
	Intrinsics      map[string]Intrinsic
}

// Deps is the loop's wiring: the four interfaces plus optional steering and log
// hooks. Steering, when set, is polled before every model call.
type Deps struct {
	Decider    Decider
	Catalog    Catalog
	Runner     Runner
	Transcript Transcript
	Steering   func() []Message
	Log        func(event string, fields map[string]any)
}

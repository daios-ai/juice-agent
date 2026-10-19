package main

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/juice"
)

// TaskSource is what intake needs from the kernel: the waiting tasks visible to
// the agent and their completion (kernel D6). The juice client satisfies it.
type TaskSource interface {
	WaitingTasks(ctx context.Context) ([]juice.Task, error)
	CompleteTask(ctx context.Context, id string, args map[string]any) error
}

// Intake is the daemon's only source of work. It polls the kernel for tasks
// whose required caller is the agent, turns each into a job in the task's
// persona, runs jobs on a bounded worker pool, and completes each task with its
// job's answer. The kernel shows the agent only tasks from requesters its
// operator approved (U52), so intake applies no access policy of its own.
//
// One active job per persona: a task for a persona already in flight waits for
// the next poll, so a conversation's messages are answered in order and never
// interleave. A persona stays busy until its task's completion is accepted or
// refused, so a later task never completes ahead of an owed one. Recovery needs no persisted claim state: the kernel's task status
// is the durable guard. A task still waiting after a restart is polled and done
// again; a completed one is never listed again.
type Intake struct {
	src      TaskSource
	run      func(context.Context, agent.Job) (agent.Outcome, error)
	address  string // the agent's own address: only tasks it must complete are its work
	budget   int
	interval time.Duration
	workers  int
	log      func(string, map[string]any)

	queue chan pending
	stop  chan struct{}
	polls sync.WaitGroup
	wg    sync.WaitGroup

	mu     sync.Mutex
	seen   map[string]bool // task ids submitted and still waiting
	active map[string]bool // personas with a job or a completion in flight
	unsent map[string]owed // task id -> completion a transient failure left undelivered
}

// owed is a completion not yet delivered, with the persona it holds busy.
type owed struct {
	persona string
	args    map[string]any
}

// pending is one queued job with what its delivery needs.
type pending struct {
	job          agent.Job
	taskID       string
	allowedInput map[string]any
}

// NewIntake builds intake over src for the agent at address, running each job
// through run with budget, on workers workers, polling every interval.
func NewIntake(src TaskSource, run func(context.Context, agent.Job) (agent.Outcome, error), address string,
	budget, workers int, interval time.Duration, log func(string, map[string]any)) *Intake {
	if log == nil {
		log = func(string, map[string]any) {}
	}
	return &Intake{src: src, run: run, address: address, budget: budget, interval: interval, workers: workers, log: log,
		queue: make(chan pending, workers*8), stop: make(chan struct{}),
		seen: map[string]bool{}, active: map[string]bool{}, unsent: map[string]owed{}}
}

// Start launches the workers and the poller. ctx bounds job execution; it is not
// cancelled on graceful shutdown, so in-flight jobs drain.
func (in *Intake) Start(ctx context.Context) {
	for i := 0; i < in.workers; i++ {
		in.wg.Add(1)
		go func() {
			defer in.wg.Done()
			for p := range in.queue {
				out, _ := in.run(ctx, p.job)
				in.deliver(p, out)
			}
		}()
	}
	in.polls.Add(1)
	go func() {
		defer in.polls.Done()
		t := time.NewTicker(in.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-in.stop:
				return
			case <-t.C:
				in.scan(ctx)
			}
		}
	}()
}

// Shutdown stops polling, then lets queued and in-flight jobs finish and their
// tasks complete (pi's drain semantics).
func (in *Intake) Shutdown() {
	close(in.stop)
	in.polls.Wait()
	close(in.queue)
	in.wg.Wait()
}

// scan resends owed completions, then queues every new task addressed to the
// agent whose persona is free.
func (in *Intake) scan(ctx context.Context) {
	in.mu.Lock()
	unsent := in.unsent
	in.unsent = map[string]owed{}
	in.mu.Unlock()
	for id, o := range unsent {
		in.complete(ctx, id, o) // the job is done; only its answer is owed
	}
	tasks, err := in.src.WaitingTasks(ctx)
	if err != nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	waiting := map[string]bool{}
	for _, t := range tasks {
		if t.RequiredCaller != in.address {
			continue // a task the agent funded but must not complete is not its work
		}
		waiting[t.ID] = true
		persona := personaKey(t)
		if in.seen[t.ID] || in.active[persona] {
			continue // deferred, not seen: a later poll takes it once the persona frees
		}
		p := pending{taskID: t.ID, allowedInput: t.AllowedInput, job: agent.Job{ID: "task-" + t.ID, Persona: persona,
			Budget: in.budget, Messages: []agent.Message{{Role: agent.RoleUser, Content: promptFromTask(t)}}}}
		select {
		case in.queue <- p:
			in.seen[t.ID], in.active[persona] = true, true
			in.log("task_received", map[string]any{"task_id": t.ID, "persona": persona})
		default:
			in.log("task_deferred", map[string]any{"task_id": t.ID, "reason": "queue full"})
		}
	}
	for id := range in.seen {
		if !waiting[id] {
			delete(in.seen, id) // no longer waiting: done, cancelled, or withheld
		}
	}
}

// deliver completes the originating task with the job's answer. Completing runs
// the task's onward action as its requester (kernel D6); for a sink the reply
// rides in the completion's args, readable by the sender. A failed job still
// completes, so the sender's process is released and the exchange never dangles.
func (in *Intake) deliver(p pending, out agent.Outcome) {
	args := out.Answer
	if args == nil {
		args = map[string]any{"status": string(out.Status)}
		if out.Reason != "" {
			args["reason"] = out.Reason
		}
	} else {
		args = shapeReply(args, p.allowedInput)
	}
	in.complete(context.Background(), p.taskID, owed{persona: p.job.Persona, args: args})
}

// complete delivers a task's completion, keeping it for the next poll when the
// failure is transient. Acceptance or a permanent refusal frees the persona: a
// refused task is no longer waiting, or stays waiting in the kernel, visible to
// the operator.
func (in *Intake) complete(ctx context.Context, id string, o owed) {
	err := in.src.CompleteTask(ctx, id, o.args)
	in.mu.Lock()
	if err != nil && juice.IsRetryable(err) {
		in.unsent[id] = o // the persona stays busy until this is delivered
	} else {
		delete(in.active, o.persona)
	}
	in.mu.Unlock()
	if err != nil {
		in.log("complete_error", map[string]any{"task_id": id, "error": err.Error()})
	}
}

// personaKey names the persona a task belongs to, from the task alone (§10):
//
//	thread_id present          → (requester, created_by, "t:"+thread_id)
//	no thread_id, direct       → (requester, created_by, "")  when requester = owner
//	no thread_id, relayed      → (requester, created_by, "task:"+id)
//
// The kernel certifies requester, owner and created_by, never a conversation, so
// the conversation comes from the application's thread_id. Without one, a
// direct sender (no intermediary between the payer and the requester) is one
// conversation, and a relayed task is its own persona: wrongly merging two
// conversations discloses, wrongly separating them only loses continuity. Each
// component is escaped before the three are joined, so the separator never
// occurs inside one and distinct keys never share a file.
func personaKey(t juice.Task) string {
	var third string
	switch thread, _ := t.PartialArgs["thread_id"].(string); {
	case thread != "":
		third = "t:" + thread
	case t.Requester != t.Owner:
		third = "task:" + t.ID
	}
	return url.QueryEscape(t.Requester) + "|" + url.QueryEscape(t.CreatedBy) + "|" + url.QueryEscape(third)
}

// promptFromTask renders a task's parked request into the job prompt, prefixed
// by who sent it: the owner, the payer the kernel certifies, and the requester
// relaying it when they differ. A "message" field (the message native's
// convention) or a "request" field is used directly; otherwise the whole
// partial_args is rendered.
func promptFromTask(t juice.Task) string {
	sender := t.Owner
	if t.Requester != t.Owner {
		sender += " via " + t.Requester
	}
	text := renderArgs(t.PartialArgs)
	if msg, ok := t.PartialArgs["message"].(string); ok && msg != "" {
		text = msg
	} else if req, ok := t.PartialArgs["request"].(string); ok && req != "" {
		text = req
	}
	return sender + ": " + text
}

// shapeReply maps the loop's reply (the reply tool's {answer: text}) onto the
// onward action's reply contract. An unconstrained onward (a sink) receives
// {answer: text}. A delivery action declaring a text-carrying field gets the
// answer under it, so the action's own schema drives the mapping and the agent
// needs no knowledge of the application. When it cannot map confidently, the
// reply passes through unchanged and the kernel validates.
func shapeReply(answer, allowed map[string]any) map[string]any {
	text, ok := answer["answer"].(string)
	if !ok {
		return answer
	}
	props, _ := allowed["properties"].(map[string]any)
	if len(props) == 0 {
		return map[string]any{"answer": text}
	}
	for _, name := range []string{"text", "message", "answer", "body", "reply", "content"} {
		if isStringProp(props, name) {
			return map[string]any{name: text}
		}
	}
	if only := onlyStringProp(props); only != "" {
		return map[string]any{only: text}
	}
	return answer
}

func isStringProp(props map[string]any, name string) bool {
	m, ok := props[name].(map[string]any)
	return ok && m["type"] == "string"
}

// onlyStringProp returns the sole string property's name, or "" if there is not
// exactly one.
func onlyStringProp(props map[string]any) string {
	found := ""
	for name, spec := range props {
		if m, ok := spec.(map[string]any); ok && m["type"] == "string" {
			if found != "" {
				return ""
			}
			found = name
		}
	}
	return found
}

// renderArgs marshals a request envelope to compact JSON for the job prompt.
func renderArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

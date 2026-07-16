// service.go is the generic notification-driven channel driver: the reference
// every dedicated service channel (chat, and future services) reuses. It owns the
// whole delivery state machine — poll the service's notification steps, read each
// conversation's new messages, submit them to the loop, post the reply, complete
// the step — plus at-least-once semantics, the read cursor, the loop guard, and
// the graceful fallback. All service-specific I/O sits behind ServiceActions, so
// this package stays kernel-free like agent/; a new service is just an adapter.
package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daios-ai/juice-agent/agent"
)

// Msg is one message from a service read: a monotonic seq (the read cursor unit),
// the sender handle, and the text.
type Msg struct {
	Seq  int64
	From string
	Text string
}

// ServiceActions is the per-channel contract a notification-driven service
// supplies; the Service driver owns everything else. An adapter (in cmd, over the
// juice client) implements it, keeping this package kernel-free.
//
//   - Pending lists the service's waiting notification steps addressed to the
//     agent (e.g. @chat doorbells), each carrying its conversation in PartialArgs.
//   - SessionKey extracts the conversation a step belongs to (e.g. the room). It is
//     also the handle passed to Read/Post and the unit of serialization.
//   - Read returns a conversation's messages newer than after (oldest first).
//   - Post delivers a reply to a conversation.
//   - Complete clears a notification step (re-arming the service's next ring).
//   - Transient reports whether an error is worth retrying (vs. permanent). The
//     adapter self-heals auth internally, so the driver never sees auth as a case.
type ServiceActions interface {
	Pending(ctx context.Context) ([]StepInfo, error)
	SessionKey(StepInfo) string
	Read(ctx context.Context, key string, after int64) ([]Msg, error)
	Post(ctx context.Context, key, text string) error
	Complete(ctx context.Context, stepID string) error
	Transient(err error) bool
}

// Service is the generic service-channel driver. It satisfies Channel.
//
// At-least-once: the per-conversation read cursor advances only after a reply is
// successfully posted (in Deliver), never at read time — so a crash mid-task, or a
// transient post failure, re-reads and re-answers rather than dropping the message
// (the accepted cost is a rare duplicate reply). One task per conversation at a
// time keeps a sender's messages ordered and non-interleaved.
type Service struct {
	name     string
	acts     ServiceActions
	handle   string // the agent's own handle, to skip its own messages
	interval time.Duration
	budget   int
	fallback string
	cursors  *cursorStore
	guard    *loopGuard // nil when the loop guard is disabled
	log      func(string, map[string]any)

	submit func(agent.Task)
	stop   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	pending map[string]route // taskID -> where the reply goes
	active  map[string]bool  // conversations with a task in flight (one at a time)
}

// route is a submitted task's destination: the conversation, the step to complete
// once the reply is posted, and the cursor value to advance to on success.
type route struct {
	session string
	stepID  string
	maxSeq  int64
}

// LoopConfig configures the per-(session,sender) loop guard. Zero fields fall back
// to the defaults (20 / 60s / 60s).
type LoopConfig struct {
	Enabled  bool
	Max      int
	Window   time.Duration
	Cooldown time.Duration
}

// ServiceConfig parameterizes a Service. Name stamps each task's channel (and
// names the cursor file); Handle is the agent's own handle; DataDir holds the
// persisted read cursor; Fallback is posted when a task yields no answer; Now
// (tests) defaults to time.Now; Log may be nil.
type ServiceConfig struct {
	Name     string
	Handle   string
	Interval time.Duration
	Budget   int
	DataDir  string
	Fallback string
	Loop     LoopConfig
	Now      func() time.Time
	Log      func(string, map[string]any)
}

// failureReply is posted when the loop produced no answer (a failure, or an empty
// answer), so a user who asked a question gets a response instead of silence.
const failureReply = "Sorry — I ran into a problem and couldn't answer that."

// NewService builds a service channel from its per-channel actions and config.
func NewService(acts ServiceActions, cfg ServiceConfig) *Service {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Fallback == "" {
		cfg.Fallback = failureReply
	}
	if cfg.Log == nil {
		cfg.Log = func(string, map[string]any) {}
	}
	s := &Service{
		name: cfg.Name, acts: acts, handle: cfg.Handle, interval: cfg.Interval,
		budget: cfg.Budget, fallback: cfg.Fallback, log: cfg.Log,
		cursors: newCursorStore(filepath.Join(cfg.DataDir, cfg.Name+"-cursors.json")),
		stop:    make(chan struct{}),
		pending: map[string]route{}, active: map[string]bool{},
	}
	if cfg.Loop.Enabled {
		s.guard = newLoopGuard(cfg.Loop.Max, cfg.Loop.Window, cfg.Loop.Cooldown, cfg.Now)
	}
	return s
}

func (s *Service) Name() string { return s.name }

func (s *Service) Start(ctx context.Context, submit func(agent.Task)) error {
	s.submit = submit
	go s.poll(ctx)
	return nil
}

func (s *Service) poll(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.scan(ctx)
		}
	}
}

// scan turns each waiting notification step into work. A step whose conversation
// is already busy is left for a later poll; a step with nothing to answer
// (malformed, empty, or only the agent's own posts) is completed straight away;
// otherwise, unless the loop guard suppresses the pair, it submits one task and
// marks the conversation busy. The read cursor is not advanced here for a
// submitted task — Deliver advances it only after the reply is posted, so an
// interrupted task re-answers rather than drops.
func (s *Service) scan(ctx context.Context) {
	steps, err := s.acts.Pending(ctx)
	if err != nil {
		return
	}
	for _, st := range steps {
		key := s.acts.SessionKey(st)
		if key == "" {
			_ = s.acts.Complete(ctx, st.ID) // malformed step: nothing to read
			continue
		}
		s.mu.Lock()
		busy := s.active[key]
		s.mu.Unlock()
		if busy {
			continue
		}
		after := s.cursors.get(key)
		msgs, err := s.acts.Read(ctx, key, after)
		if err != nil {
			if !s.acts.Transient(err) { // permanent (e.g. room gone): give up on this step
				s.log("service_read_failed", map[string]any{"channel": s.name, "session": key, "error": err.Error()})
				_ = s.acts.Complete(ctx, st.ID)
			}
			continue // transient: leave the step waiting for the next poll
		}
		fresh, maxSeq := freshFromOthers(msgs, after, s.handle)
		if len(fresh) == 0 { // only our own / already-read messages: nothing to say
			s.cursors.set(key, maxSeq)
			_ = s.acts.Complete(ctx, st.ID)
			continue
		}
		if s.guard != nil && !s.guard.allow(key+"\x1f"+fresh[len(fresh)-1].From) {
			s.log("service_loop_suppressed", map[string]any{"channel": s.name, "session": key, "from": fresh[len(fresh)-1].From})
			s.cursors.set(key, maxSeq)
			_ = s.acts.Complete(ctx, st.ID)
			continue
		}
		taskID := s.name + "-" + st.ID
		s.mu.Lock()
		s.active[key] = true
		s.pending[taskID] = route{session: key, stepID: st.ID, maxSeq: maxSeq}
		s.mu.Unlock()
		s.submit(agent.Task{ID: taskID, Channel: s.name, Budget: s.budget,
			Messages: []agent.Message{{Role: agent.RoleUser, Content: renderMsgs(fresh)}}})
	}
}

// Deliver posts the task's reply and, on success, advances the read cursor and
// completes the step (re-arming the next ring). It always posts something — the
// answer, or the fallback when the task produced none — so a user is never left in
// silence. On a transient post failure it leaves the step waiting and the cursor
// unadvanced, so the next poll re-answers (at-least-once); on a permanent failure
// it gives up (complete + advance + log). Freeing the conversation always happens.
func (s *Service) Deliver(taskID string, out agent.Outcome) {
	s.mu.Lock()
	r, ok := s.pending[taskID]
	delete(s.pending, taskID)
	if ok {
		delete(s.active, r.session)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	ctx := context.Background()
	if err := s.acts.Post(ctx, r.session, s.replyText(out)); err != nil {
		if s.acts.Transient(err) {
			s.log("service_post_retry", map[string]any{"channel": s.name, "session": r.session, "error": err.Error()})
			return // leave the step + cursor; the next poll re-reads and re-answers
		}
		s.log("service_post_failed", map[string]any{"channel": s.name, "session": r.session, "error": err.Error()})
	}
	s.cursors.set(r.session, r.maxSeq)
	_ = s.acts.Complete(ctx, r.stepID)
}

// Steering is unused by service channels in v1.
func (s *Service) Steering(string) []agent.Message { return nil }

func (s *Service) Stop() { s.once.Do(func() { close(s.stop) }) }

// replyText is the text to post: the agent's answer, or the graceful fallback when
// the task failed or produced an empty answer.
func (s *Service) replyText(out agent.Outcome) string {
	if out.Answer != nil {
		if txt, ok := out.Answer["answer"].(string); ok && strings.TrimSpace(txt) != "" {
			return txt
		}
	}
	return s.fallback
}

// freshFromOthers keeps messages newer than after not sent by the agent itself (so
// it never answers its own posts), and returns the largest seq seen so the cursor
// can advance past everything read.
func freshFromOthers(msgs []Msg, after int64, handle string) (fresh []Msg, maxSeq int64) {
	maxSeq = after
	for _, m := range msgs {
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
		if m.Seq > after && !SameHandle(m.From, handle) {
			fresh = append(fresh, m)
		}
	}
	return fresh, maxSeq
}

// renderMsgs formats the new messages as one sender-labelled prompt.
func renderMsgs(msgs []Msg) string {
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s: %s", m.From, m.Text)
	}
	return b.String()
}

// cursorStore persists the per-conversation read cursor (the largest message seq
// the agent has processed) so a restart never re-answers old messages. Writes are
// atomic (temp file + rename) so a crash mid-write leaves the prior cursor intact.
type cursorStore struct {
	mu   sync.Mutex
	path string
	m    map[string]int64
}

func newCursorStore(path string) *cursorStore {
	c := &cursorStore{path: path, m: map[string]int64{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c.m)
	}
	return c
}

func (c *cursorStore) get(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

func (c *cursorStore) set(key string, seq int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seq <= c.m[key] {
		return
	}
	c.m[key] = seq
	b, err := json.Marshal(c.m)
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, c.path)
	}
}

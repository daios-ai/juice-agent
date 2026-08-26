package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice-agent/channels"
)

// chatActions must satisfy the driver's per-channel contract.
var _ channels.ServiceActions = (*chatActions)(nil)

// errGrant is the fake kernel's grant_required rejection.
var errGrant = errors.New("grant required")

// fakeKernel is an in-memory channels.Kernel. chat/msg/* actions fail with
// errGrant until the chat/msg grant is connected, so the grant self-heal is
// exercised; chat/register mints a canned key + verification step.
type fakeKernel struct {
	steps      []channels.StepInfo
	connectors []string
	messages   []any // chat/msg/read result payload
	completed  map[string]bool
	grants     map[string]string
	registered int
}

func newFakeKernel() *fakeKernel {
	return &fakeKernel{completed: map[string]bool{}, grants: map[string]string{}}
}

func (f *fakeKernel) hasGrant() bool {
	for _, d := range f.connectors {
		if d == chatGrant {
			return true
		}
	}
	return false
}

func (f *fakeKernel) WaitingSteps(context.Context) ([]channels.StepInfo, error) {
	return f.steps, nil
}
func (f *fakeKernel) CompleteStep(_ context.Context, id string, _ map[string]any) error {
	f.completed[id] = true
	return nil
}
func (f *fakeKernel) Run(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	switch action {
	case chatHandle + "/register":
		f.registered++
		return map[string]any{"api_key": "key-1", "step_id": "verify-1"}, nil
	case chatHandle + "/msg/read":
		if !f.hasGrant() {
			return nil, errGrant
		}
		return map[string]any{"messages": f.messages}, nil
	case chatHandle + "/msg/message":
		if !f.hasGrant() {
			return nil, errGrant
		}
		return map[string]any{}, nil
	}
	return map[string]any{}, nil
}
func (f *fakeKernel) Connectors(context.Context) ([]string, error) { return f.connectors, nil }
func (f *fakeKernel) ConnectGrant(_ context.Context, selector, token string) error {
	f.connectors = append(f.connectors, selector)
	f.grants[selector] = token
	return nil
}
func (f *fakeKernel) Transient(err error) bool     { return false }
func (f *fakeKernel) GrantRequired(err error) bool { return errors.Is(err, errGrant) }

func TestChatNewBuildsNamedService(t *testing.T) {
	svc := New(newFakeKernel(), channels.ServiceConfig{Handle: "agent", Interval: time.Hour, Budget: 20, DataDir: t.TempDir()})
	if svc.Name() != "chat" {
		t.Fatalf("Name() = %q, want chat", svc.Name())
	}
}

func TestChatPendingFiltersChat(t *testing.T) {
	fk := newFakeKernel()
	fk.steps = []channels.StepInfo{
		{ID: "d1", OwnerHandle: "chat", PartialArgs: map[string]any{"room": "r1"}},
		{ID: "x1", OwnerHandle: "alice", PartialArgs: map[string]any{"message": "hi"}},
	}
	a := &chatActions{k: fk, handle: "agent"}
	got, err := a.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "d1" {
		t.Fatalf("pending = %+v, want only the chat doorbell d1", got)
	}
	if a.SessionKey(got[0]) != "r1" {
		t.Fatalf("session key = %q, want r1", a.SessionKey(got[0]))
	}
}

func TestChatReadParsesMessages(t *testing.T) {
	fk := newFakeKernel()
	fk.connectors = []string{chatGrant} // already granted
	fk.messages = []any{
		map[string]any{"seq": float64(3), "from": "alice", "text": "hello"},
	}
	a := &chatActions{k: fk, handle: "agent"}
	msgs, err := a.Read(context.Background(), "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Seq != 3 || msgs[0].From != "alice" || msgs[0].Text != "hello" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestChatEnsureGrantProvisionsWhenAbsent(t *testing.T) {
	fk := newFakeKernel() // no connectors
	a := &chatActions{k: fk, handle: "agent"}
	if err := a.ensureGrant(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fk.registered != 1 {
		t.Fatalf("register called %d times, want 1", fk.registered)
	}
	if !fk.completed["verify-1"] {
		t.Fatal("verification step must be completed (activates the key)")
	}
	if fk.grants[chatGrant] != "key-1" {
		t.Fatalf("grant token = %q, want key-1", fk.grants[chatGrant])
	}
}

func TestChatEnsureGrantIdempotent(t *testing.T) {
	fk := newFakeKernel()
	fk.connectors = []string{chatGrant} // already connected
	a := &chatActions{k: fk, handle: "agent"}
	if err := a.ensureGrant(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fk.registered != 0 {
		t.Fatal("already-granted agent must not re-register")
	}
}

func TestChatReadReprovisionsOnGrantRequired(t *testing.T) {
	fk := newFakeKernel() // no grant yet -> first read fails grant_required
	fk.messages = []any{map[string]any{"seq": float64(1), "from": "alice", "text": "hey"}}
	a := &chatActions{k: fk, handle: "agent"}
	msgs, err := a.Read(context.Background(), "r1", 0)
	if err != nil {
		t.Fatalf("read should self-heal the grant and succeed, got %v", err)
	}
	if fk.registered != 1 || !fk.hasGrant() {
		t.Fatalf("read must provision the grant on grant_required (registered=%d granted=%v)", fk.registered, fk.hasGrant())
	}
	if len(msgs) != 1 || msgs[0].Text != "hey" {
		t.Fatalf("messages after reprovision = %+v", msgs)
	}
}

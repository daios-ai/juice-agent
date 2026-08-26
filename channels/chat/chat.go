// Package chat is the agent's chat channel: a dedicated notification-driven service
// over juice-ui's chat rooms, and the reference for how a service channel is
// built. It supplies only the chat-specific pieces — how to list chat doorbells,
// read a room, post a reply, complete a doorbell, key a conversation by room, and
// provision the chat/msg grant — and hands them to the generic channels.Service
// driver, which owns the delivery loop, at-least-once cursor, and reply loop guard.
// It speaks the kernel only through channels.Kernel, so it stays kernel-free; the
// cmd adapter implements that interface over the juice client.
package chat

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/daios-ai/juice-agent/channels"
)

// chatHandle is the juice-ui chat service's user handle; it owns the doorbell steps
// that notify members of new messages. Its member actions live in the msg/ folder
// (chat/msg/read, chat/msg/message) under one grant selector (chatGrant); the open
// chat/register sits outside that folder.
const (
	chatHandle = "chat"
	chatGrant  = "chat/msg"
)

// New builds the chat channel: the generic Service driver configured with chat's
// actions. cfg carries the poll interval, budget, data dir, context window,
// loop-guard settings, and logger; Name is forced to "chat".
func New(k channels.Kernel, cfg channels.ServiceConfig) *channels.Service {
	cfg.Name = "chat"
	return channels.NewService(&chatActions{k: k, handle: cfg.Handle}, cfg)
}

// chatActions implements channels.ServiceActions over chat's kernel actions, and
// provisions the chat/msg grant on demand so enabling chat is only a config flag: a
// first-run, absent, or revoked grant self-heals lazily on the first grant_required
// from a read or post. The mutex serializes concurrent provisioning.
type chatActions struct {
	k      channels.Kernel
	handle string
	mu     sync.Mutex
}

func (a *chatActions) Pending(ctx context.Context) ([]channels.StepInfo, error) {
	steps, err := a.k.WaitingSteps(ctx)
	if err != nil {
		return nil, err
	}
	var out []channels.StepInfo
	for _, s := range steps {
		if s.OwnerHandle == chatHandle { // chat owns the doorbells; the agent is their required caller
			out = append(out, s)
		}
	}
	return out, nil
}

func (a *chatActions) SessionKey(s channels.StepInfo) string {
	room, _ := s.PartialArgs["room"].(string)
	return room
}

// Read fetches a room's new messages, provisioning the grant and retrying once if
// the kernel reports the agent has not yet connected chat/msg.
func (a *chatActions) Read(ctx context.Context, room string, after int64) ([]channels.Msg, error) {
	out, err := a.readOnce(ctx, room, after)
	if a.k.GrantRequired(err) {
		if gerr := a.ensureGrant(ctx); gerr != nil {
			return nil, err
		}
		out, err = a.readOnce(ctx, room, after)
	}
	return out, err
}

func (a *chatActions) readOnce(ctx context.Context, room string, after int64) ([]channels.Msg, error) {
	res, err := a.k.Run(ctx, chatHandle+"/msg/read", map[string]any{"room": room, "after": after})
	if err != nil {
		return nil, err
	}
	raw, _ := res["messages"].([]any)
	out := make([]channels.Msg, 0, len(raw))
	for _, mi := range raw {
		m, _ := mi.(map[string]any)
		out = append(out, channels.Msg{Seq: asInt64(m["seq"]), From: asString(m["from"]), Text: asString(m["text"])})
	}
	return out, nil
}

// Post delivers a reply, provisioning the grant and retrying once on grant_required
// (the same self-heal as Read).
func (a *chatActions) Post(ctx context.Context, room, text string) error {
	err := a.postOnce(ctx, room, text)
	if a.k.GrantRequired(err) {
		if gerr := a.ensureGrant(ctx); gerr != nil {
			return err
		}
		err = a.postOnce(ctx, room, text)
	}
	return err
}

func (a *chatActions) postOnce(ctx context.Context, room, text string) error {
	_, err := a.k.Run(ctx, chatHandle+"/msg/message", map[string]any{"room": room, "text": text})
	return err
}

func (a *chatActions) Complete(ctx context.Context, stepID string) error {
	return a.k.CompleteStep(ctx, stepID, nil)
}

func (a *chatActions) Transient(err error) bool { return a.k.Transient(err) }

// ensureGrant makes sure the agent holds the chat/msg grant. It is idempotent: if
// the agent already lists the chat/msg connector it returns at once. Otherwise it
// runs the headless path the browser's "Connect to chat" button uses — register the
// agent's handle for a pending key, complete the verification step (which the kernel
// lets only the handle's owner do, proving identity), then attach the key as the
// chat/msg grant.
func (a *chatActions) ensureGrant(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	dirs, err := a.k.Connectors(ctx)
	if err != nil {
		return err
	}
	if hasConnector(dirs, chatGrant) {
		return nil // another caller (or a prior run) already connected it
	}
	reg, err := a.k.Run(ctx, chatHandle+"/register", map[string]any{"username": a.handle})
	if err != nil {
		return err
	}
	apiKey, stepID := asString(reg["api_key"]), asString(reg["step_id"])
	if apiKey == "" || stepID == "" {
		return fmt.Errorf("chat register: empty api_key or step_id")
	}
	if err := a.k.CompleteStep(ctx, stepID, nil); err != nil { // owner completes -> activates the key
		return err
	}
	return a.k.ConnectGrant(ctx, chatGrant, apiKey)
}

// hasConnector reports whether a held grant directory covers selector — the exact
// folder, or one nested under it. The provisioning idempotency check.
func hasConnector(dirs []string, selector string) bool {
	for _, d := range dirs {
		if d == selector || strings.HasPrefix(d, selector+"/") {
			return true
		}
	}
	return false
}

// asInt64 coerces a JSON number (float64) or integer to int64.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func asString(v any) string { s, _ := v.(string); return s }

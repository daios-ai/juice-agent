package channels

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/daios-ai/juice-agent/agent"
)

// convTurn is one persisted visible turn: a role (user or assistant) and its text.
// Only the conversation the room actually sees is stored — the incoming messages
// and the agent's posted answers — never the agent's tool calls, lookups, or
// reasoning (those live in the per-job transcript, not here).
type convTurn struct {
	Role string `json:"role"` // "user" | "assistant"
	Text string `json:"text"`
}

// convStore persists a bounded window of recent conversation turns per session so
// each new job can be seeded with the room's recent history — the conversation
// continuity of design §4. It mirrors cursorStore: a per-session JSON map under the
// data dir, written atomically (temp + rename). A non-positive window disables it,
// so newConvStore returns nil and every method is nil-safe.
type convStore struct {
	mu     sync.Mutex
	path   string
	window int // max turns retained per session
	m      map[string][]convTurn
}

// newConvStore opens (or starts) the store at path, keeping the last window turns
// per session. window <= 0 returns nil — the feature is off and callers treat a nil
// store as "no context" (single-message jobs, as before).
func newConvStore(path string, window int) *convStore {
	if window <= 0 {
		return nil
	}
	c := &convStore{path: path, window: window, m: map[string][]convTurn{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c.m)
	}
	return c
}

// seed returns the session's stored turns as role-tagged messages, oldest first, to
// prepend to a new job's Messages. A nil store (disabled) or an empty history
// returns nil.
func (c *convStore) seed(key string) []agent.Message {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	turns := c.m[key]
	if len(turns) == 0 {
		return nil
	}
	out := make([]agent.Message, 0, len(turns))
	for _, t := range turns {
		out = append(out, agent.Message{Role: agent.Role(t.Role), Content: t.Text})
	}
	return out
}

// append records one exchange — the incoming message that was answered and the
// answer posted back to the room — trims to the window, and persists. A nil store
// is a no-op. Callers append only after the reply is actually posted, so the store
// mirrors exactly what appeared in the room.
func (c *convStore) append(key, userText, answerText string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	turns := append(c.m[key],
		convTurn{Role: string(agent.RoleUser), Text: userText},
		convTurn{Role: string(agent.RoleAssistant), Text: answerText})
	if len(turns) > c.window {
		turns = append([]convTurn(nil), turns[len(turns)-c.window:]...) // keep the most recent window
	}
	c.m[key] = turns
	b, err := json.Marshal(c.m)
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, c.path)
	}
}

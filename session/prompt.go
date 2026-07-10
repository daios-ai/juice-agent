package session

import (
	"fmt"
	"os"
	"strings"

	"github.com/daios-ai/juice-agent/agent"
)

// Prompter assembles the per-task system prompt from the agent's authored
// persona and prior knowledge plus its learned-memory digest. It is pi's
// buildSystemPrompt, transposed: one builder, no per-model variation (the kernel
// is the one provider). Absent sections are simply omitted.
type Prompter struct {
	Handle      string // the agent, e.g. "@research"
	ReplyAction string // the terminal action, e.g. "@research/reply"
	Persona     string // persona.md contents (may be empty)
	Knowledge   string // knowledge.md contents (may be empty)
}

// LoadPrompter builds a Prompter, reading the persona and knowledge files (a
// missing or unreadable file yields an empty section, never an error).
func LoadPrompter(handle, replyAction, personaPath, knowledgePath string) *Prompter {
	return &Prompter{
		Handle:      handle,
		ReplyAction: replyAction,
		Persona:     readFileOrEmpty(personaPath),
		Knowledge:   readFileOrEmpty(knowledgePath),
	}
}

// Build returns the system message to prepend to a task, folding in the current
// memory digest. It always includes a role line and the operating contract;
// persona, knowledge, and memory appear only when non-empty.
func (p *Prompter) Build(memoryDigest string) agent.Message {
	handle := p.Handle
	if handle == "" {
		handle = "@agent"
	}
	reply := p.ReplyAction
	if reply == "" {
		reply = handle + "/reply"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, an agent on the Juice platform. You solve each task by selecting "+
		"actions from the catalog, one per turn, and finish by selecting %s with your answer.\n",
		handle, reply)
	if s := strings.TrimSpace(p.Persona); s != "" {
		b.WriteString("\n# Persona\n" + s + "\n")
	}
	if s := strings.TrimSpace(p.Knowledge); s != "" {
		b.WriteString("\n# Knowledge\n" + s + "\n")
	}
	if s := strings.TrimSpace(memoryDigest); s != "" {
		b.WriteString("\n# Memory (what you learned from past tasks)\n" + s + "\n")
	}
	b.WriteString("\n# Operating\nPrefer actions with a proven track record; try an untested one " +
		"only when nothing proven fits. Stay within your credit budget. When you have the answer, " +
		"select " + reply + ".")
	return agent.Message{Role: agent.RoleSystem, Kind: "system", Content: strings.TrimSpace(b.String())}
}

func readFileOrEmpty(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

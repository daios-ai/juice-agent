package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/daios-ai/juice-agent/agent"
)

// systemPrompt assembles the system message that begins every job: a role line,
// the operator's agent.md (persona and knowledge) when non-empty, and the
// operating contract. It is pi's buildSystemPrompt, transposed: one builder, no
// per-model variation, since the kernel is the one provider.
func systemPrompt(handle, replyTool, agentMD string) agent.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, an agent on the Juice platform. You solve each task by selecting one "+
		"action or tool per turn, and finish by selecting the %q tool with your answer.\n", handle, replyTool)
	if s := strings.TrimSpace(agentMD); s != "" {
		b.WriteString("\n# Agent\n" + s + "\n")
	}
	b.WriteString("\n# Operating\nEarlier turns of this conversation, if any, come before the current " +
		"request; each incoming message names who sent it. You can run only the actions offered to you; " +
		"run the action lookup to find more. Prefer actions whose record shows successful use; try one with no record " +
		"only when nothing recorded fits. For a multi-step sub-problem, open the subtask tool with a " +
		"specific instruction — it works in its own scope and returns only its result. Stay within " +
		"your credit budget. When you have the answer, select the " + replyTool + " tool.")
	return agent.Message{Role: agent.RoleSystem, Kind: "system", Content: strings.TrimSpace(b.String())}
}

// readFileOrEmpty returns path's contents, or "" when it is missing or unreadable:
// an absent agent.md contributes nothing, never an error.
func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// Command juice-agent is the Juice agent daemon: it acts as one login on a Juice
// kernel, takes its work from the kernel's tasks, and runs the pi-style loop over
// the kernel action catalog to serve each job in the task's persona — paying for
// what it runs and rating its tools.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/juice"
)

// version and commit are injected at build time via -ldflags (see Makefile).
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	// The verb comes first; the agent's login is the positional that follows it.
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "serve":
		err = runServe(rest)
	case "task":
		err = runTask(rest)
	case "bench":
		err = runBench(rest)
	case "version", "--version", "-v":
		fmt.Printf("juice-agent %s (%s)\n", version, commit)
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// commands is the usage table: each command line as typed, and what it does.
var commands = [][2]string{
	{"serve <login>", "run the daemon: answer the tasks addressed to the login"},
	{`task <login> "<prompt>"`, "run one job and print its outcome"},
	{"bench <login> <suite.json> [N]", "run the experiment suite and write a report"},
	{"version", "print version and commit"},
}

// usageNote follows the table. Paragraphs are unbroken so they wrap to any width;
// indented lines are commands and stay as written.
const usageNote = "A login is an account on a kernel, handle@kernel, held by the juice CLI:\n" +
	"  juice kernel add <url>\n" +
	"  juice auth login <handle>@<kernel>\n\n" +
	"The agent's home is $JUICE_HOME/agents/<login>/ (JUICE_HOME default ~/.juice), holding " +
	"agent.md, an optional config.json, and personas/."

func usage() { fmt.Fprint(os.Stderr, usageText(helpWidth(term.GetSize))) }

// usageText renders the usage at width columns. Each command line stays whole, since a
// wrapped command no longer pastes; its description wraps below it, indented.
func usageText(width int) string {
	var b strings.Builder
	b.WriteString("usage:\n")
	for _, c := range commands {
		b.WriteString("  juice-agent " + c[0] + "\n      " + wrapText(c[1], width, 6) + "\n")
	}
	b.WriteString("\n" + wrapText(usageNote, width, 0) + "\n")
	return b.String()
}

// maxHelpWidth is the column usage is written for, the conventional terminal width.
const maxHelpWidth = 80

// helpWidth is the column usage wraps at: stderr's width, where usage is printed, capped
// at maxHelpWidth since long lines read worse even on a wide screen; maxHelpWidth when
// stderr is not a terminal. size is term.GetSize, a parameter so the rule is testable
// without a terminal.
func helpWidth(size func(fd int) (width, height int, err error)) int {
	if w, _, err := size(int(os.Stderr.Fd())); err == nil && w > 0 {
		return min(w, maxHelpWidth)
	}
	return maxHelpWidth
}

// wrapText fills s to width columns, word by word, counting runes. Its first line is taken to start
// at column indent, where the caller has already written the text before it, and every later line is
// indented to match. A blank line or one starting with a space (a list, an aligned table) is kept as
// written; a word longer than the room left stays whole on a line of its own.
func wrapText(s string, width, indent int) string {
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteString("\n" + strings.Repeat(" ", indent))
		}
		if line == "" || line[0] == ' ' {
			b.WriteString(line)
			continue
		}
		col := indent
		for j, word := range strings.Fields(line) {
			n := utf8.RuneCountInString(word)
			if j > 0 && col+1+n > width {
				b.WriteString("\n" + strings.Repeat(" ", indent))
				col = indent
			} else if j > 0 {
				b.WriteString(" ")
				col++
			}
			b.WriteString(word)
			col += n
		}
	}
	return b.String()
}

// setup does the shared startup: resolve config, open the login's session,
// check that it is the account the login names, and assemble the executor. It
// returns the executor, the client, and the resolved config.
func setup(ctx context.Context, log *logger, login string) (*Executor, *juice.Client, Config, error) {
	cfg, err := ResolveConfig(login)
	if err != nil {
		return nil, nil, cfg, fmt.Errorf("config: %w", err)
	}
	if err := cfg.EnsureHome(); err != nil {
		return nil, nil, cfg, fmt.Errorf("ensure home %s: %w", cfg.Home, err)
	}
	cl, err := juice.New(login, juiceHome(), cfg.Reason, cfg.ActionLookup)
	if err != nil {
		return nil, nil, cfg, err
	}
	self, err := cl.Me(ctx)
	if err != nil {
		return nil, nil, cfg, fmt.Errorf("identify %s: %w", login, err)
	}
	// The credential file's name is a label; the account behind it is the
	// principal it records. A renamed or recreated account must not be served
	// under memory and a home that belong to another.
	if self.Address != login || (cl.Principal() != "" && self.ID != cl.Principal()) {
		return nil, nil, cfg, fmt.Errorf("the session for %s belongs to %s (principal %s): run juice auth login %s",
			login, self.Address, self.ID, login)
	}
	log.event("login", map[string]any{"login": login, "available": self.Available})
	// The two affordances must meet their contracts; the action lookup is then
	// offered with the terms the kernel holds for it.
	actionLookup, err := cl.Affordances(ctx)
	if err != nil {
		return nil, nil, cfg, fmt.Errorf("config.json: %w", err)
	}

	// Harness tools: plain-named, intercepted, never registered on the platform.
	loopCfg := agent.Config{
		ReplyTool:      "reply",
		SubtaskTool:    "subtask",
		ActionLookup:   cfg.ActionLookup,
		Tools:          []agent.Tool{replyTool(), subtaskTool()},
		Always:         []agent.Candidate{actionLookup},
		MaxTurns:       cfg.MaxTurns,
		MaxRepeats:     maxRepeats,
		CandidateLimit: candidateLimit,
		LookupLimit:    lookupLimit,
		Intrinsics: map[string]agent.Intrinsic{"reply": {Handle: func(_ context.Context, args map[string]any) (map[string]any, bool, error) {
			return args, true, nil // terminal: the answer is the selection's args
		}}},
	}
	log.event("identity", map[string]any{"login": login, "home": cfg.Home, "reason": cfg.Reason, "action_lookup": cfg.ActionLookup})
	exec := &Executor{
		Kernel:       cl,
		PersonasDir:  cfg.PersonasDir,
		CompactBytes: cfg.CompactBytes,
		Retry:        DefaultRetryPolicy(),
		RetryOK:      juice.IsRetryable,
		Loop:         loopCfg,
		RateTasks:    cfg.RateTasks,
		Handle:       login,
		AgentPath:    cfg.agentPath(),
		Log:          log.event,
	}
	return exec, cl, cfg, nil
}

// The loop's tuning: how many identical repeats end a frame, how many candidate
// actions it offers, and how many rows a lookup returns. They shape the loop, not
// the operator's policy, so they are constants rather than configuration.
const (
	maxRepeats     = 3
	candidateLimit = 24
	lookupLimit    = 8
)

// runServe serves as the login in args[0].
func runServe(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: juice-agent serve <login>")
	}
	log := newLogger()
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exec, cl, cfg, err := setup(runCtx, log, args[0])
	if err != nil {
		return err
	}

	if marked, err := Recover(cfg.PersonasDir); err == nil && len(marked) > 0 {
		log.event("recovery", map[string]any{"interrupted": marked})
	}
	in := NewIntake(cl, exec.Execute, cfg.Login, cfg.DefaultBudget, cfg.Concurrency,
		time.Duration(cfg.PollSeconds)*time.Second, log.event)
	in.Start(runCtx)
	log.event("serving", map[string]any{"concurrency": cfg.Concurrency, "poll_seconds": cfg.PollSeconds})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.event("draining", nil)
	in.Shutdown()
	log.event("stopped", nil)
	return nil
}

// runTask runs one job as the login in args[0] with the prompt in args[1], in
// the persona "cli", so one-shot runs continue one conversation of their own.
func runTask(args []string) error {
	if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
		return fmt.Errorf(`usage: juice-agent task <login> "<prompt>"`)
	}
	login, prompt := args[0], args[1]
	log := newLogger()
	ctx := context.Background()
	exec, _, cfg, err := setup(ctx, log, login)
	if err != nil {
		return err
	}
	job := agent.Job{
		ID:       "cli-" + time.Now().Format("20060102-150405"),
		Persona:  "cli",
		Budget:   cfg.DefaultBudget,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: prompt}},
	}
	out, err := exec.Execute(ctx, job)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// logger writes structured JSON log lines to stderr (stdout stays reserved for
// resource payloads, per the kernel's logging discipline). Every line carries a
// timestamp, an event name, and the event's fields.
type logger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newLogger() *logger {
	return &logger{enc: json.NewEncoder(os.Stderr)}
}

// event emits one structured log line. A nil fields map is fine.
func (l *logger) event(event string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "event": event}
	for k, v := range fields {
		rec[k] = v
	}
	l.enc.Encode(rec)
}

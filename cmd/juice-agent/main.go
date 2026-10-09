// Command juice-agent is the Juice agent daemon: it acts as one login on a Juice
// kernel, watches its intake channels, and runs the pi-style loop over the kernel
// action catalog to serve each job — paying for what it runs, rating its tools,
// and (optionally) publishing new actions to automate recurring work.
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
	"github.com/daios-ai/juice-agent/channels"
	"github.com/daios-ai/juice-agent/channels/chat"
	httpchan "github.com/daios-ai/juice-agent/channels/http"
	"github.com/daios-ai/juice-agent/channels/task"
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
	{"serve <login>", "run the daemon (channels + worker pool)"},
	{`task <login> "<prompt>"`, "run one job and print its outcome"},
	{"bench <login> <suite.json> [N]", "run the experiment suite and write a report"},
	{"version", "print version and commit"},
}

// usageNote follows the table. Paragraphs are unbroken so they wrap to any width;
// indented lines are commands and stay as written.
const usageNote = "A login is an account on a kernel, handle@kernel, held by the juice CLI:\n" +
	"  juice kernel add <url>\n" +
	"  juice auth login <handle>@<kernel>\n\n" +
	"The agent's home is $JUICE_HOME/agents/<login>/ (JUICE_HOME default ~/.juice), holding an " +
	"optional config.json, persona.md, knowledge.md, memory.md, and data/."

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
	cl, err := juice.New(login, juiceHome())
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

	maker, err := NewMaker(cfg.DataDir, cfg.makerConfig(), cl, cl, cl.Sys("tinygo/compile"), log.event)
	if err != nil {
		return nil, nil, cfg, fmt.Errorf("maker: %w", err)
	}

	// Harness tools: plain-named, intercepted, never registered on the platform.
	intrinsics := map[string]agent.Intrinsic{
		"reply": {Handle: func(_ context.Context, args map[string]any) (map[string]any, bool, error) {
			return args, true, nil // terminal: the answer is the selection's args
		}},
		"publish": maker.PublishIntrinsic(),
	}
	loopCfg := agent.Config{
		ReplyTool:      "reply",
		SubtaskTool:    "subtask",
		LookupAction:   cl.Sys("lookup"),
		Tools:          []agent.Tool{replyTool(), subtaskTool(), publishTool()},
		Always:         []agent.Candidate{lookupCandidate(cl.Sys("lookup"))},
		MaxTurns:       cfg.MaxTurns,
		MaxRepeats:     cfg.MaxRepeats,
		CandidateLimit: cfg.CandidateLimit,
		LookupLimit:    cfg.LookupLimit,
		Intrinsics:     intrinsics,
	}
	prompter := LoadPrompter(login, "reply", cfg.personaPath(), cfg.knowledgePath())
	var mem *Memory
	if cfg.MemoryBytes > 0 {
		mem = NewMemory(cfg.memoryPath(), cfg.MemoryBytes, cl, "kernel="+cl.KernelKey()+" principal="+cl.Principal(), log.event)
	}
	log.event("identity", map[string]any{"login": login, "home": cfg.Home, "memory": mem != nil})

	exec := &Executor{
		Kernel:       cl,
		DataDir:      cfg.DataDir,
		CompactBytes: cfg.CompactBytes,
		Retry:        DefaultRetryPolicy(),
		RetryOK:      juice.IsRetryable,
		Loop:         loopCfg,
		RateTasks:    cfg.RateTasks,
		Maker:        maker,
		Prompter:     prompter,
		Memory:       mem,
		Log:          log.event,
	}
	return exec, cl, cfg, nil
}

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

	if marked, err := Recover(cfg.DataDir); err == nil && len(marked) > 0 {
		log.event("recovery", map[string]any{"interrupted": marked})
	}

	chans := buildChannels(cfg, cl, log.event)
	rt := NewRuntime(exec, chans, cfg.Concurrency, log.event)
	if err := rt.Start(runCtx, cfg.Concurrency); err != nil {
		return err
	}
	fields := map[string]any{"channels": channelNames(chans), "concurrency": cfg.Concurrency}
	for _, ch := range chans {
		if hc, ok := ch.(*httpchan.HTTP); ok {
			fields["http_addr"] = hc.Addr
		}
	}
	log.event("serving", fields)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.event("draining", nil)
	rt.Shutdown()
	log.event("stopped", nil)
	return nil
}

// runTask runs one job as the login in args[0] with the prompt in args[1].
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
		Channel:  "cli",
		Budget:   cfg.DefaultBudget,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: prompt}},
	}
	out, err := exec.Execute(ctx, job, nil)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// buildChannels constructs the enabled intake surfaces from config. One
// kernelAdapter is the single kernel bridge shared by every channel: the task
// channel takes it as a channels.TaskSource, chat as the full channels.Kernel. A
// dedicated service channel (chat) claims the tasks its service owns, so that
// owner is excluded from the task channel. Adding a channel is one case here plus
// a package under channels/ (see requirements §10).
func buildChannels(cfg Config, cl *juice.Client, log func(string, map[string]any)) []channels.Channel {
	k := kernelAdapter{cl: cl}
	var out []channels.Channel
	var exclude []string
	chatOn := cfg.Channels.Chat != nil && cfg.Channels.Chat.Enabled
	if chatOn {
		exclude = append(exclude, cl.Address("chat"))
	}
	if hc := cfg.Channels.HTTP; hc != nil && hc.Addr != "" {
		out = append(out, httpchan.NewHTTP(hc.Addr, cfg.DefaultBudget))
	}
	interval := time.Duration(cfg.Channels.Task.PollSeconds) * time.Second
	out = append(out, task.New(k, interval, cfg.DefaultBudget, cfg.Login, exclude...))
	if chatOn {
		cc := cfg.Channels.Chat
		out = append(out, chat.New(k, cl.Address("chat"), channels.ServiceConfig{
			Address:         cfg.Login,
			Interval:        time.Duration(cc.PollSeconds) * time.Second,
			Budget:          cfg.DefaultBudget,
			DataDir:         cfg.DataDir,
			ContextMessages: cc.ContextMessages,
			Loop: channels.LoopConfig{
				Enabled:  cc.Loop.Enabled,
				Max:      cc.Loop.MaxEvents,
				Window:   time.Duration(cc.Loop.WindowSeconds) * time.Second,
				Cooldown: time.Duration(cc.Loop.CooldownSeconds) * time.Second,
			},
			Log: log,
		}))
	}
	return out
}

func channelNames(cs []channels.Channel) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name())
	}
	return out
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

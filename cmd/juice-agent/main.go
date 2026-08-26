// Command juice-agent is the Juice agent daemon: it logs into a Juice kernel as a
// user, watches its intake channels, and runs the pi-style loop over the kernel
// action catalog to serve each task — paying for what it runs, rating its tools,
// and (optionally) publishing new actions to automate recurring work.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/channels"
	"github.com/daios-ai/juice-agent/channels/chat"
	httpchan "github.com/daios-ai/juice-agent/channels/http"
	"github.com/daios-ai/juice-agent/channels/step"
	"github.com/daios-ai/juice-agent/juice"
	"golang.org/x/term"
)

// version and commit are injected at build time via -ldflags (see Makefile).
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	// --password-stdin may appear anywhere; extract it before subcommand dispatch.
	stdinPass := false
	args := extractFlag(os.Args[1:], "--password-stdin", &stdinPass)

	// The verb comes first; the agent's handle is the positional that follows it.
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "serve":
		err = runServe(rest, stdinPass)
	case "task":
		err = runTask(rest, stdinPass)
	case "bench":
		err = runBench(rest, stdinPass)
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

// extractFlag removes every occurrence of a boolean flag from args, setting
// *set if it was present. CLI parsing here is manual (no flag package), so this
// keeps `--password-stdin` position-independent.
func extractFlag(args []string, name string, set *bool) []string {
	out := args[:0]
	for _, a := range args {
		if a == name {
			*set = true
			continue
		}
		out = append(out, a)
	}
	return out
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  juice-agent serve [handle]                  run the daemon (channels + worker pool)
  juice-agent task <handle> "<prompt>"        run one task and print its outcome
  juice-agent bench <handle> <suite.json>     run the experiment suite and write a report
  juice-agent version                         print version and commit

Each agent lives in its own home under $JUICE_HOME/agent/agents/<handle>/ (JUICE_HOME default
~/.juice), holding config.json, persona.md, knowledge.md, memory.md, and data/. Handles are bare;
serve takes it as its argument, else AGENT_HANDLE, else agent.
password (in precedence order): --password-stdin, AGENT_PASSWORD, AGENT_PASSWORD_FILE, or an
interactive prompt when run in a terminal.`)
}

// setup does the shared startup: load config, authenticate, bootstrap the
// intrinsic actions, and assemble the executor. It returns the executor, the
// client, and the resolved loop config.
func setup(ctx context.Context, log *logger, cliHandle string, stdinPass bool) (*Executor, *juice.Client, agent.Config, Config, error) {
	cfg, err := ResolveConfig(cliHandle)
	if err != nil {
		return nil, nil, agent.Config{}, cfg, fmt.Errorf("config: %w", err)
	}
	if notice := legacyNotice(); notice != "" {
		log.event("legacy-notice", map[string]any{"message": notice})
	}
	if err := cfg.EnsureHome(); err != nil {
		return nil, nil, agent.Config{}, cfg, fmt.Errorf("ensure home %s: %w", cfg.Home, err)
	}
	password, err := resolvePassword(cfg.Handle, stdinPass)
	if err != nil {
		return nil, nil, agent.Config{}, cfg, err
	}
	cl := juice.New(cfg.ServerURL, cfg.Handle, password)
	if err := cl.Login(ctx); err != nil {
		return nil, nil, agent.Config{}, cfg, fmt.Errorf("login as %s: %w", cfg.Handle, err)
	}
	self, err := cl.Me(ctx)
	if err != nil {
		return nil, nil, agent.Config{}, cfg, fmt.Errorf("identify self: %w", err)
	}
	log.event("login", map[string]any{"handle": self.Handle, "available": self.Available})

	maker, err := NewMaker(cfg.DataDir, cfg.makerConfig(), cl, cl, log.event)
	if err != nil {
		return nil, nil, agent.Config{}, cfg, fmt.Errorf("maker: %w", err)
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
		LookupAction:   "sys/lookup",
		Tools:          []agent.Tool{replyTool(), subtaskTool(), publishTool()},
		Always:         []agent.Candidate{lookupCandidate()},
		MaxTurns:       cfg.MaxTurns,
		MaxRepeats:     cfg.MaxRepeats,
		CandidateLimit: cfg.CandidateLimit,
		LookupLimit:    cfg.LookupLimit,
		Intrinsics:     intrinsics,
	}
	log.event("bootstrap", map[string]any{"handle": self.Handle, "tools": []string{"reply", "subtask", "publish"}})
	prompter := LoadPrompter(self.Handle, "reply", cfg.personaPath(), cfg.knowledgePath())
	var mem *Memory
	if cfg.Memory.Enabled {
		mem = NewMemory(cfg.memoryPath(), cfg.Memory.MemoryBytes, cl)
	}
	log.event("identity", map[string]any{"handle": self.Handle, "home": cfg.Home, "memory": cfg.Memory.Enabled})

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
	return exec, cl, loopCfg, cfg, nil
}

// runServe serves as args[0], or as the default agent when no handle is given.
func runServe(args []string, stdinPass bool) error {
	handle := ""
	if len(args) > 0 {
		handle = args[0]
	}
	log := newLogger()
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exec, cl, _, cfg, err := setup(runCtx, log, handle, stdinPass)
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

// runTask runs one task as args[0] with the prompt in args[1].
func runTask(args []string, stdinPass bool) error {
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return fmt.Errorf(`usage: juice-agent task <handle> "<prompt>"`)
	}
	handle, prompt := args[0], args[1]
	log := newLogger()
	ctx := context.Background()
	exec, _, _, cfg, err := setup(ctx, log, handle, stdinPass)
	if err != nil {
		return err
	}
	task := agent.Task{
		ID:       "cli-" + time.Now().Format("20060102-150405"),
		Channel:  "cli",
		Budget:   cfg.DefaultBudget,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: prompt}},
	}
	out, err := exec.Execute(ctx, task, nil)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// chatHandle is the sender the chat channel claims; the generic step channel
// excludes it so the two partition the agent's steps without overlap.
const chatHandle = "chat"

// buildChannels constructs the enabled intake surfaces from config. One
// kernelAdapter is the single kernel bridge shared by every channel: the step
// channel takes it as a channels.StepSource, chat as the full channels.Kernel. A
// dedicated service channel (chat) claims its own steps, so its sender handle is
// excluded from the step channel. Adding a channel is one case here plus a package
// under channels/ (see requirements §10).
func buildChannels(cfg Config, cl *juice.Client, log func(string, map[string]any)) []channels.Channel {
	k := kernelAdapter{cl: cl}
	var out []channels.Channel
	var exclude []string
	chatOn := cfg.Channels.Chat != nil && cfg.Channels.Chat.Enabled
	if chatOn {
		exclude = append(exclude, chatHandle)
	}
	if hc := cfg.Channels.HTTP; hc != nil && hc.Addr != "" {
		out = append(out, httpchan.NewHTTP(hc.Addr, cfg.DefaultBudget))
	}
	if sc := cfg.Channels.Step; sc != nil {
		interval := time.Duration(sc.PollSeconds) * time.Second
		out = append(out, step.NewStep(k, interval, cfg.DefaultBudget, cfg.Handle, exclude...))
	}
	if chatOn {
		cc := cfg.Channels.Chat
		out = append(out, chat.New(k, channels.ServiceConfig{
			Handle:          cfg.Handle,
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

// resolvePassword reads the daemon's password by a precedence chain, from
// non-interactive to interactive (a bare --password flag is intentionally not
// offered — it would leak via shell history and /proc/<pid>/cmdline). Secrets
// never live in config.
func resolvePassword(handle string, stdinFlag bool) (string, error) {
	if stdinFlag {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading --password-stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if p := os.Getenv("AGENT_PASSWORD"); p != "" {
		return p, nil
	}
	if f := os.Getenv("AGENT_PASSWORD_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("reading AGENT_PASSWORD_FILE: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "Password for %s: ", handle)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("no password: use --password-stdin, set AGENT_PASSWORD or AGENT_PASSWORD_FILE, or run in a terminal")
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

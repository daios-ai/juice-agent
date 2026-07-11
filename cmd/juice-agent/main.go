// Command juice-agent is the Juice agent daemon: it logs into a Juice kernel as a
// user, watches its intake channels, and runs the pi-style loop over the kernel
// action catalog to serve each task — paying for what it runs, rating its tools,
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

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/juice"
)

// version and commit are injected at build time via -ldflags (see Makefile).
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	args := os.Args[1:]
	// An optional leading @handle names the agent to launch: `juice-agent @research serve`.
	handle := ""
	if len(args) > 0 && strings.HasPrefix(args[0], "@") {
		handle = args[0]
		args = args[1:]
	}
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(rest, handle)
	case "task":
		err = runTask(rest, handle)
	case "bench":
		err = runBench(rest, handle)
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

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  juice-agent [@handle] serve                 run the daemon (channels + worker pool)
  juice-agent [@handle] task "<prompt>"       run one task and print its outcome
  juice-agent [@handle] bench <suite.json>    run the experiment suite and write a report

A leading @handle launches that named agent from its home (<agents_dir>/<handle>/).
config: AGENT_CONFIG (default ./agent.json); password: AGENT_PASSWORD or AGENT_PASSWORD_FILE`)
}

// setup does the shared startup: load config, authenticate, bootstrap the
// intrinsic actions, and assemble the executor. It returns the executor, the
// client, and the resolved loop config.
func setup(ctx context.Context, log *logger, cliHandle string) (*Executor, *juice.Client, agent.Config, Config, error) {
	cfg, err := ResolveConfig(cliHandle)
	if err != nil {
		return nil, nil, agent.Config{}, cfg, fmt.Errorf("config: %w", err)
	}
	password, err := resolvePassword()
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
		LookupAction:   "@sys/lookup",
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

func runServe(_ []string, handle string) error {
	log := newLogger()
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exec, cl, _, cfg, err := setup(runCtx, log, handle)
	if err != nil {
		return err
	}

	if marked, err := Recover(cfg.DataDir); err == nil && len(marked) > 0 {
		log.event("recovery", map[string]any{"interrupted": marked})
	}

	channels := buildChannels(cfg, cl)
	rt := NewRuntime(exec, channels, cfg.Concurrency, log.event)
	if err := rt.Start(runCtx, cfg.Concurrency); err != nil {
		return err
	}
	fields := map[string]any{"channels": channelNames(channels), "concurrency": cfg.Concurrency}
	for _, ch := range channels {
		if hc, ok := ch.(*HTTP); ok {
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

func runTask(args []string, handle string) error {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("usage: juice-agent task \"<prompt>\"")
	}
	log := newLogger()
	ctx := context.Background()
	exec, _, _, cfg, err := setup(ctx, log, handle)
	if err != nil {
		return err
	}
	task := agent.Task{
		ID:       "cli-" + time.Now().Format("20060102-150405"),
		Channel:  "cli",
		Budget:   cfg.DefaultBudget,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: args[0]}},
	}
	out, err := exec.Execute(ctx, task, nil)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// buildChannels constructs the enabled intake surfaces from config.
func buildChannels(cfg Config, cl *juice.Client) []Channel {
	var out []Channel
	if hc := cfg.Channels.HTTP; hc != nil && hc.Addr != "" {
		out = append(out, NewHTTP(hc.Addr, cfg.DefaultBudget))
	}
	if sc := cfg.Channels.Step; sc != nil {
		interval := time.Duration(sc.PollSeconds) * time.Second
		out = append(out, NewStep(stepAdapter{cl: cl}, interval, cfg.DefaultBudget))
	}
	return out
}

func channelNames(cs []Channel) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name())
	}
	return out
}

func configPath() string {
	if p := os.Getenv("AGENT_CONFIG"); p != "" {
		return p
	}
	return "./agent.json"
}

// resolvePassword reads the daemon's password from AGENT_PASSWORD, else from the
// file named by AGENT_PASSWORD_FILE. Secrets never live in config.
func resolvePassword() (string, error) {
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
	return "", fmt.Errorf("no password: set AGENT_PASSWORD or AGENT_PASSWORD_FILE")
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

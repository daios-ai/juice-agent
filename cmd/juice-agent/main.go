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
	"syscall"
	"time"

	"github.com/daios-ai/juice-agent/agent"
	"github.com/daios-ai/juice-agent/channel"
	"github.com/daios-ai/juice-agent/juice"
	"github.com/daios-ai/juice-agent/session"
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
func setup(ctx context.Context, log *logger, cliHandle string) (*session.Executor, *juice.Client, agent.Config, Config, error) {
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

	replyIn, replyOut := replySchema()
	replyRef, err := ensureAction(ctx, cl, "reply", replyIn, replyOut)
	if err != nil {
		return nil, nil, agent.Config{}, cfg, fmt.Errorf("bootstrap @agent/reply: %w", err)
	}
	pubIn, pubOut := publishSchema()
	publishRef, err := ensureAction(ctx, cl, "publish", pubIn, pubOut)
	if err != nil {
		return nil, nil, agent.Config{}, cfg, fmt.Errorf("bootstrap @agent/publish: %w", err)
	}
	log.event("bootstrap", map[string]any{"reply": replyRef, "publish": publishRef})

	maker, err := session.NewMaker(cfg.DataDir, cfg.makerConfig(), cl, cl, log.event)
	if err != nil {
		return nil, nil, agent.Config{}, cfg, fmt.Errorf("maker: %w", err)
	}

	intrinsics := map[string]agent.Intrinsic{
		replyRef: {Handle: func(_ context.Context, args map[string]any) (map[string]any, bool, error) {
			return args, true, nil // terminal: the answer is the selection's args
		}},
		publishRef: maker.PublishIntrinsic(),
	}
	loopCfg := agent.Config{
		ReplyAction:    replyRef,
		LookupAction:   "@sys/lookup",
		AlwaysRefs:     []string{"@sys/lookup"},
		MaxTurns:       cfg.MaxTurns,
		CandidateLimit: cfg.CandidateLimit,
		LookupLimit:    cfg.LookupLimit,
		Intrinsics:     intrinsics,
	}
	prompter := session.LoadPrompter(self.Handle, replyRef, cfg.personaPath(), cfg.knowledgePath())
	var mem *session.Memory
	if cfg.Memory.Enabled {
		mem = session.NewMemory(cfg.memoryPath(), cfg.Memory.MemoryBytes, cl)
	}
	log.event("identity", map[string]any{"handle": self.Handle, "home": cfg.Home, "memory": cfg.Memory.Enabled})

	exec := &session.Executor{
		Kernel:       cl,
		DataDir:      cfg.DataDir,
		CompactBytes: cfg.CompactBytes,
		Retry:        session.DefaultRetryPolicy(),
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

	if marked, err := session.Recover(cfg.DataDir); err == nil && len(marked) > 0 {
		log.event("recovery", map[string]any{"interrupted": marked})
	}

	channels := buildChannels(cfg, cl)
	rt := NewRuntime(exec, channels, cfg.Concurrency, log.event)
	if err := rt.Start(runCtx, cfg.Concurrency); err != nil {
		return err
	}
	fields := map[string]any{"channels": channelNames(channels), "concurrency": cfg.Concurrency}
	for _, ch := range channels {
		if hc, ok := ch.(*channel.HTTP); ok {
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
func buildChannels(cfg Config, cl *juice.Client) []channel.Channel {
	var out []channel.Channel
	if hc := cfg.Channels.HTTP; hc != nil && hc.Addr != "" {
		out = append(out, channel.NewHTTP(hc.Addr, cfg.DefaultBudget))
	}
	if sc := cfg.Channels.Step; sc != nil {
		interval := time.Duration(sc.PollSeconds) * time.Second
		out = append(out, channel.NewStep(stepAdapter{cl: cl}, interval, cfg.DefaultBudget))
	}
	return out
}

func channelNames(cs []channel.Channel) []string {
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

// Command ritam is the local multi-agent orchestrator.
//
// It starts a terminal UI that lets a human submit a task to an authoritative
// agent, watch that agent deliberate with supporting agents through the
// orchestrator, and intervene at any point.
//
// The scaffold ships with mock agents enabled, so `ritam` runs end to end with
// no Claude Code, no Codex, no network and no API keys.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"ritam/internal/app"
	"ritam/internal/brand"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
	"ritam/internal/tui"
)

func main() {
	if ok, err := runCommand(os.Args[1:], os.Stdout); ok {
		if err != nil {
			fmt.Fprintln(os.Stderr, brand.Name+":", err)
			os.Exit(1)
		}
		return
	}
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags]\n", brand.Name)
		flag.PrintDefaults()
		fmt.Fprintln(flag.CommandLine.Output(), commandUsage())
	}
	var (
		configPath = flag.String("config", "", "the project's settings file (its folder is the project); "+
			"default: the folder "+brand.Name+" is started in")
		demo     = flag.Bool("demo", false, "create and run the built-in demo task on start")
		headless = flag.Bool("headless", false, "run the demo without the TUI and print the event log")
		real     = flag.Bool("real", false, "use the real Claude Code / Codex CLIs instead of mock agents")
		mock     = flag.Bool("mock", false, "force the offline mock agents, overriding the config")
		title    = flag.String("task", "", "with -headless: the task to run instead of the built-in demo")
	)
	flag.Parse()

	if err := run(*configPath, *demo, *headless, *real, *mock, *title); err != nil {
		fmt.Fprintln(os.Stderr, brand.Name+":", err)
		os.Exit(1)
	}
}

func run(configPath string, demo, headless, useReal, useMock bool, taskTitle string) error {
	cfg, pl, err := prepare(configPath)
	if err != nil {
		return err
	}

	if useMock {
		// An explicit request for mocks wins over anything in the config.
		cfg.TUI.UseMockAgents = true
	}
	if useReal && useMock {
		return fmt.Errorf("-real and -mock are mutually exclusive")
	}

	if useReal {
		// Real CLIs are slow compared to mocks: a single investigation can
		// take minutes, so the idle timeout must not fire mid-answer.
		cfg.TUI.UseMockAgents = false
		if cfg.Orchestrator.IdleTimeout < 10*time.Minute {
			cfg.Orchestrator.IdleTimeout = 10 * time.Minute
		}
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Logging.File), 0o700); err != nil {
		return err
	}
	log, err := logging.New(logging.Options{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
		File:   cfg.Logging.File,
	})
	if err != nil {
		return err
	}
	defer log.Close()
	log.Info("starting", "project", pl.proj.Root, "projectId", pl.proj.ID, "home", pl.home.Root)
	for _, n := range pl.notices {
		log.Info("notice", "text", n)
	}

	sys, err := app.Build(cfg, log)
	if err != nil {
		return err
	}
	defer sys.Close()
	if info, err := sys.Service.Access(context.Background()); err == nil && info.HeldBack != "" {
		log.Warn("access held back", "reason", info.HeldBack)
		pl.notices = append(pl.notices, "access "+info.HeldBack)
	}

	// The local LLM proxy is optional and disabled by default. It is not the
	// orchestrator: it only handles provider traffic.
	if sys.Proxy.Enabled() {
		if err := sys.Proxy.Start(context.Background()); err != nil {
			log.Warn("llm proxy not started", "error", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if headless {
		printNotices(os.Stdout, pl.notices)
		return runHeadlessDemo(ctx, sys, taskTitle)
	}

	return tui.Run(ctx, sys.Service, tui.Options{
		DemoOnStart:      demo || cfg.TUI.DemoOnStart,
		DefaultAuthority: domain.AgentID(cfg.Orchestrator.DefaultAuthority),
		WorkspaceNotice:  sys.Workspace.Notice,
		Notices:          pl.notices,
	})
}

// runHeadlessDemo exercises the full mock orchestration flow and prints the
// task's event log. It exists so the architecture can be verified without a
// terminal, which is also how the end-to-end test reads.
func runHeadlessDemo(ctx context.Context, sys *app.System, taskTitle string) error {
	svc := sys.Service

	agents, err := svc.Agents(ctx)
	if err != nil {
		return err
	}
	if len(agents) < 2 && sys.Config.TUI.UseMockAgents {
		// The team starts empty; the scripted demo recruits two mock agents.
		for _, in := range []ports.RecruitInput{
			{Name: "demo-lead", Provider: "claude-code", Model: "opus", Role: domain.RoleAuthority},
			{Name: "demo-helper", Provider: "codex", Model: "gpt-6-sol"},
		} {
			if _, err := svc.Recruit(ctx, in); err != nil {
				return err
			}
		}
		if agents, err = svc.Agents(ctx); err != nil {
			return err
		}
	}
	if len(agents) < 2 {
		return fmt.Errorf("the demo needs at least two agents, found %d: recruit them in the TUI first", len(agents))
	}

	var authority domain.AgentID
	participants := make([]domain.AgentID, 0, len(agents))
	for _, a := range agents {
		participants = append(participants, a.ID)
		if a.Role == domain.RoleAuthority && authority == "" {
			authority = a.ID
		}
	}
	if authority == "" {
		authority = participants[0]
	}

	title := "Fix the authentication cookie bug"
	description := "Logged-out users keep an authenticated session cookie."
	if taskTitle != "" {
		title, description = taskTitle, ""
	}

	task, err := svc.CreateTask(ctx, ports.CreateTaskInput{
		Title:        title,
		Description:  description,
		Authority:    authority,
		Participants: participants,
	})
	if err != nil {
		return err
	}
	fmt.Printf("created %s  authority=%s  participants=%v\n\n", task.ID, task.AuthorityAgent, task.Participants)

	if err := svc.RunTask(ctx, task.ID); err != nil {
		return err
	}

	events, err := svc.TaskEvents(ctx, task.ID)
	if err != nil {
		return err
	}
	for _, e := range events {
		fmt.Println(e.String())
	}

	final, err := svc.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s final state: %s\n", final.ID, final.State)

	ds, err := svc.TaskDecisions(ctx, task.ID)
	if err != nil {
		return err
	}
	for _, d := range ds {
		fmt.Printf("%s [%s] %s\n", d.ID, d.Status, d.Decision)
	}
	return nil
}

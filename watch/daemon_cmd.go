package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/config"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/daemon"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/dispatch"
	tmuxfe "github.com/matteobortolazzo/cenci/watch/v2/internal/frontend/tmux"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/ipc"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/logging"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/tmux"
)

// daemonRestartReadyTimeout bounds how long `daemon restart` waits for the
// freshly spawned daemon to become reachable before reporting failure. It
// mirrors internal/daemon's EnsureRunning readyTimeout default.
const daemonRestartReadyTimeout = 3 * time.Second

// daemonRestartPollInterval is the polling cadence within daemonRestartReadyTimeout.
const daemonRestartPollInterval = 50 * time.Millisecond

// runDaemonGroup implements `cenci daemon [start|stop|restart|status]`.
// A bare `daemon` (no args) or `daemon` followed only by flags acts as
// `start`, so `cenci daemon -v` still works exactly as the old bare
// `cenci -v` did.
func runDaemonGroup(args []string) {
	if len(args) == 0 {
		runDaemonStart(args)
		return
	}
	switch args[0] {
	case "start":
		runDaemonStart(args[1:])
	case "stop":
		runDaemonStop(args[1:])
	case "restart":
		runDaemonRestart(args[1:])
	case "status":
		runDaemonStatus(args[1:])
	default:
		if strings.HasPrefix(args[0], "-") {
			runDaemonStart(args)
			return
		}
		fmt.Fprintf(os.Stderr, "cenci daemon: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// runDaemonStart runs the daemon loop in the foreground — identical to the
// pre-#daemon-lifecycle bare `cenci [-flags]` invocation, plus PID-file
// bookkeeping: a PID file is written at ipc.DefaultPIDPath() once this
// process has become the one live daemon (never on the "already running"
// no-op path — see daemon.Run's onStarted contract) and removed on clean
// shutdown.
func runDaemonStart(args []string) {
	cfg := config.Default()
	fs := flag.NewFlagSet("daemon start", flag.ExitOnError)

	fs.BoolVar(&cfg.Verbose, "v", false, "verbose logging")
	fs.BoolVar(&cfg.LogJSON, "json", os.Getenv("CENCI_LOG_JSON") == "1", "emit structured JSON log lines instead of plain text (default from CENCI_LOG_JSON)")
	fs.StringVar(&cfg.SocketPath, "socket", ipc.DefaultSocketPath(), "IPC broadcast socket path (empty to disable)")
	fs.StringVar(&cfg.EventSocketPath, "event-socket", ipc.DefaultEventSocketPath(), "event socket path for hook notifications")

	sweepSec := int(cfg.SweepInterval / time.Second)
	fs.IntVar(&sweepSec, "sweep", sweepSec, "stale session sweep interval in seconds")
	fs.DurationVar(&cfg.SessionTTL, "session-ttl", cfg.SessionTTL, "idle expiry for sessions outside tmux (e.g. 2h)")

	fs.StringVar(&cfg.StyleIdle, "style-idle", cfg.StyleIdle, "tmux style for idle state")
	fs.StringVar(&cfg.StyleRunning, "style-running", cfg.StyleRunning, "tmux style for running state")
	fs.StringVar(&cfg.StyleDone, "style-done", cfg.StyleDone, "tmux style for done state")
	fs.StringVar(&cfg.StyleNeedInput, "style-input", cfg.StyleNeedInput, "tmux style for need-input state")
	fs.StringVar(&cfg.SymbolIdle, "symbol-idle", cfg.SymbolIdle, "symbol prefix for idle state")
	fs.StringVar(&cfg.SymbolRunning, "symbol-running", cfg.SymbolRunning, "symbol prefix for running state")
	fs.StringVar(&cfg.SymbolDone, "symbol-done", cfg.SymbolDone, "symbol prefix for done state")
	fs.StringVar(&cfg.SymbolNeedInput, "symbol-input", cfg.SymbolNeedInput, "symbol prefix for need-input state")
	fs.StringVar(&cfg.StyleStopped, "style-stopped", cfg.StyleStopped, "tmux style for stopped (interrupted) state")
	fs.StringVar(&cfg.SymbolStopped, "symbol-stopped", cfg.SymbolStopped, "symbol prefix for stopped (interrupted) state")
	_ = fs.Parse(args)

	cfg.SweepInterval = time.Duration(sweepSec) * time.Second

	logger := logging.New(os.Stderr, cfg.LogJSON)

	if cfg.Verbose {
		logger.Log(logging.SeverityInfo, "", fmt.Sprintf("cenci starting (event-driven, sweep every %s)", cfg.SweepInterval))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		if cfg.Verbose {
			logger.Log(logging.SeverityInfo, "", fmt.Sprintf("received %s, shutting down", sig))
		}
		cancel()
	}()

	// tmux is the one interactive frontend; it is constructed here and
	// injected so the daemon core stays tmux-free.
	fe := tmuxfe.New(cfg, &tmux.ExecClient{})

	// The embedded dispatch + reconcile loop always runs alongside the daemon
	// (#220); its own per-tick config reload decides whether dispatch.loopEnabled
	// is on, so a disabled config simply idles the loop rather than gating
	// whether it starts at all.
	ch := make(chan ipc.AttentionUpdate, 1)
	var attention <-chan ipc.AttentionUpdate = ch
	go dispatch.RunCombinedLoop(ctx, "", &tmux.ExecClient{}, &dispatch.GHMutator{}, os.Stdout, ch)

	pidPath := ipc.DefaultPIDPath()
	started := false
	onStarted := func() {
		started = true
		if err := daemon.WritePIDFile(pidPath); err != nil {
			logger.Log(logging.SeverityWarn, "", fmt.Sprintf("warning: could not write pid file %s: %v", pidPath, err))
		}
	}
	defer func() {
		if started {
			daemon.RemovePIDFile(pidPath)
		}
	}()

	if err := daemon.Run(ctx, cfg, fe, attention, onStarted); err != nil {
		fmt.Fprintf(os.Stderr, "cenci: %v\n", err)
		os.Exit(1)
	}
}

// runDaemonStop implements `cenci daemon stop`. Exits 0 whether or not
// a daemon was actually stopped (idempotent) — the message on stdout reports
// which.
func runDaemonStop(args []string) {
	fs := flag.NewFlagSet("daemon stop", flag.ExitOnError)
	_ = fs.Parse(args)
	rejectExtra("cenci daemon stop", fs.Args())

	outcome, err := daemon.Stop(ipc.DefaultEventSocketPath(), ipc.DefaultPIDPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "cenci daemon stop: %v\n", err)
		os.Exit(1)
	}
	if outcome.WasRunning {
		fmt.Printf("stopped daemon (pid %d)\n", outcome.PID)
		return
	}
	fmt.Println("daemon not running")
}

// runDaemonRestart implements `cenci daemon restart`: stop (if running),
// then spawn a fresh detached daemon the same way EnsureRunning does
// (daemon.Spawn — `cenci daemon start`, Setsid'd), and wait briefly for
// it to become reachable.
func runDaemonRestart(args []string) {
	fs := flag.NewFlagSet("daemon restart", flag.ExitOnError)
	_ = fs.Parse(args)
	rejectExtra("cenci daemon restart", fs.Args())

	outcome, err := daemon.Stop(ipc.DefaultEventSocketPath(), ipc.DefaultPIDPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "cenci daemon restart: %v\n", err)
		os.Exit(1)
	}
	if outcome.WasRunning {
		fmt.Printf("stopped daemon (pid %d)\n", outcome.PID)
	}

	daemon.Spawn()

	deadline := time.Now().Add(daemonRestartReadyTimeout)
	for time.Now().Before(deadline) {
		if daemon.Alive(ipc.DefaultEventSocketPath()) {
			fmt.Println("daemon restarted")
			return
		}
		time.Sleep(daemonRestartPollInterval)
	}
	fmt.Fprintln(os.Stderr, "cenci daemon restart: daemon did not become ready in time")
	os.Exit(1)
}

// runDaemonStatus implements `cenci daemon status`: a narrow
// running/not-running + PID report, distinct from the broader `cenci
// status` overview (sessions + dispatch loop). Exits 1 when the daemon is
// not running so scripts can branch on it; exits 0 when running.
func runDaemonStatus(args []string) {
	fs := flag.NewFlagSet("daemon status", flag.ExitOnError)
	_ = fs.Parse(args)
	rejectExtra("cenci daemon status", fs.Args())

	info := daemon.Status(ipc.DefaultEventSocketPath(), ipc.DefaultPIDPath())
	if !info.Running {
		fmt.Println("daemon not running")
		os.Exit(1)
	}
	if info.PID > 0 {
		fmt.Printf("daemon running (pid %d)\n", info.PID)
	} else {
		fmt.Println("daemon running (pid unknown)")
	}
}
